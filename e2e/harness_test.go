//go:build e2e

// Package e2e is the black-box end-to-end suite (issue #20): the control worker
// via Helm in kind, the data worker as its OCI container on the host, dev
// Temporal on the kind network, and mhvtl as the library.
//
// Topology:
//
//	kind cluster (docker network "kind")
//	 ├─ control-worker Deployment  ← Helm chart, control-worker image
//	 │     ROLE=control → temporal:7233
//	 └─ temporal container (joined to the kind network, alias "temporal")
//	host:
//	 ├─ data-worker container  --network kind, tape-device + /dev/zfs passthrough,
//	 │     pool bind mount (recovery set baked into the image) → temporal:7233
//	 └─ this test process: blank-tape prep, run submission, and one shared mock
//	      webhook (reachable from the cluster via the kind bridge gateway IP)
//
// The suite skips cleanly when any prerequisite is absent (issue #20 AC5).
// Driven by `make test-e2e`.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

const (
	clusterName  = "tape-archiver-e2e"
	helmRelease  = "ta-control"
	kindNetwork  = "kind"
	namespace    = "default"
	imageVersion = "e2e"
	controlRepo  = "ghcr.io/soliddowant/tape-archiver/control-worker"
	dataRepo     = "ghcr.io/soliddowant/tape-archiver/data-worker"

	// temporalAlias gives Temporal a stable "temporal:7233" on the kind network.
	temporalAlias = "temporal"
	temporalPort  = "7233"

	// poolMount is bind-mounted rshared so .zfs/snapshot/ automounts propagate.
	poolMount = "/mnt/tape-test-pool"
	// containerStagingDir is TAPE_STAGING_DIR in the data container: a host dir,
	// not the tiny test pool, which the test reads for AC4 slice corruption.
	containerStagingDir = "/staging"

	// datasetParent is democratic-csi's datasetParentName (K8sDatasetParent) for
	// the k8s-source test.
	datasetParent = "tape_test"

	// scaledJobRelease is the autoscaling tests' own ScaledJob-shaped release.
	scaledJobRelease = "ta-control-sj"

	// The KEDA Temporal scaler needs KEDA >= 2.17. KEDA is a hard prerequisite so
	// autoscaling is never silently skipped.
	kedaNamespace    = "keda"
	kedaRelease      = "keda"
	kedaChartName    = "keda"
	kedaChartVersion = "2.21.0"
	// kedaChartRepo is used via --repo so no global helm repo entry is added.
	kedaChartRepo = "https://kedacore.github.io/charts"
)

// orderedPhases is the full phase sequence (SPEC §4.3). Burn always runs, as a
// recorded no-op when burning is disabled.
var orderedPhases = []string{
	backup.PhaseResolve,
	backup.PhasePrepare,
	backup.PhasePack,
	backup.PhaseGeneratePAR2,
	backup.PhaseVerify,
	backup.PhaseLoad,
	backup.PhaseWrite,
	backup.PhaseEject,
	backup.PhaseReport,
	backup.PhaseBurn,
	backup.PhaseDeliver,
}

var (
	harness *e2eHarness
	// skipReason is set (harness nil) when a prerequisite is missing (AC5).
	skipReason string
)

// e2eHarness owns the suite's cluster, containers, mock webhook and LIFO cleanups.
type e2eHarness struct {
	repoRoot       string
	kubeconfig     string
	tapectlPath    string // built tapectl binary used to submit runs (the operator path)
	stagingHostDir string // host staging dir bind-mounted into the data container
	temporalCID    string
	dataCID        string
	gatewayIP      string // host address reachable from the kind network
	webhookURL     string // base URL, e.g. http://172.18.0.1:39000
	rec            *recorder

	// opticalDevices are loop-device pseudo-burners; empty when losetup is absent.
	opticalDevices  []string
	opticalBackings []string

	cleanups []func()
}

func TestMain(m *testing.M) {
	if reason := checkPrerequisites(); reason != "" {
		skipReason = reason
		log.Printf("[e2e] skipping suite: %s", reason)
		os.Exit(m.Run())
	}

	reason, err := probeDriveReadiness()
	if err != nil {
		log.Printf("[e2e] tape drive probe failed: %v", err)
		os.Exit(1)
	}

	if reason != "" {
		skipReason = reason
		log.Printf("[e2e] skipping suite: %s", reason)
		os.Exit(m.Run())
	}

	h, err := setupHarness()
	if err != nil {
		log.Printf("[e2e] setup failed: %v", err)
		h.teardown()
		os.Exit(1)
	}

	harness = h
	code := m.Run()

	h.teardown()
	os.Exit(code)
}

// requireHarness returns the harness, or skips when a prerequisite is missing.
func requireHarness(t *testing.T) *e2eHarness {
	t.Helper()

	if harness == nil {
		t.Skipf("e2e prerequisites unavailable: %s", skipReason)
	}

	return harness
}

// checkPrerequisites returns a reason to skip, or "". It mirrors testutil.SkipIf*
// without the *testing.T that TestMain lacks.
func checkPrerequisites() string {
	for _, tool := range []string{
		"docker", "kind", "helm", "kubectl", "nix", "make",
		"age", "age-keygen", "zstd", "mkltfs", "ltfs", "zfs", "mt", "sg_raw",
	} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Sprintf("%q not on PATH (run within `nix develop`)", tool)
		}
	}

	if err := exec.Command("docker", "info").Run(); err != nil {
		return "docker daemon not reachable"
	}

	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		return "TEMPORAL_ADDRESS not set (run via `make test-e2e`)"
	}

	for _, dev := range []string{envOr("MHVTL_CHANGER_DEV", "/dev/sch0"), "/dev/fuse", "/dev/zfs"} {
		if _, err := os.Stat(dev); err != nil {
			return fmt.Sprintf("device %s not present", dev)
		}
	}

	if _, err := os.Stat(envOr("TAPE_POOL_MOUNT", poolMount+"/archive")); err != nil {
		return "ZFS test pool not mounted (run `make zpool-up`)"
	}

	if os.Getenv("TAPE_TEST_SNAPSHOT") == "" {
		return "TAPE_TEST_SNAPSHOT not set (run via `make test-e2e`)"
	}

	return ""
}

// setupHarness brings the system up, registering teardown per resource so a
// mid-setup failure still unwinds.
func setupHarness() (*e2eHarness, error) {
	h := &e2eHarness{rec: newRecorder()}

	_, thisFile, _, _ := runtime.Caller(0)
	h.repoRoot = filepath.Dir(filepath.Dir(thisFile))

	steps := []struct {
		name string
		fn   func() error
	}{
		{"build-images", h.buildImages},
		{"build-tapectl", h.buildTapectl},
		{"kind-cluster", h.createCluster},
		{"keda", h.installKeda},
		{"load-control-image", h.loadControlImage},
		{"snapshot-crds", h.installSnapshotCRDs},
		{"temporal-on-kind-net", h.joinTemporalToNetwork},
		{"mock-webhook", h.startWebhook},
		{"clean-leftover-workflows", h.cleanLeftoverWorkflows},
		{"staging-dir", h.createStagingDir},
		{"optical-discs", h.setupOpticalDiscs},
		{"deploy-control-worker", h.deployControlWorker},
		{"data-worker-container", h.startDataWorker},
	}

	for _, step := range steps {
		start := time.Now()

		log.Printf("[e2e] setup %s: starting", step.name)

		if err := step.fn(); err != nil {
			return h, fmt.Errorf("setup %s: %w", step.name, err)
		}

		log.Printf("[e2e] setup %s: done in %s", step.name, time.Since(start).Round(time.Second))
	}

	return h, nil
}

func (h *e2eHarness) buildImages() error {
	_, err := execOut(h.repoRoot, nil, "make", "build-images", "VERSION="+imageVersion)

	return err
}

// buildTapectl builds tapectl; tests submit runs through it, the operator path.
func (h *e2eHarness) buildTapectl() error {
	h.tapectlPath = filepath.Join(os.TempDir(), "tape-archiver-e2e-tapectl")

	_, err := execOut(h.repoRoot, nil, "go", "build", "-o", h.tapectlPath, "./cmd/tapectl")
	if err != nil {
		return err
	}

	h.push(func() { _ = os.Remove(h.tapectlPath) })

	return nil
}

func (h *e2eHarness) createCluster() error {
	h.kubeconfig = filepath.Join(os.TempDir(), "tape-archiver-e2e.kubeconfig")

	_, _ = execOut(h.repoRoot, nil, "kind", "delete", "cluster", "--name", clusterName)

	if _, err := execOut(h.repoRoot, nil, "kind", "create", "cluster",
		"--name", clusterName,
		"--config", filepath.Join("e2e", "kind-config.yaml"),
		"--kubeconfig", h.kubeconfig,
		"--wait", "120s",
	); err != nil {
		return err
	}

	h.push(func() {
		_, _ = execOut(h.repoRoot, nil, "kind", "delete", "cluster", "--name", clusterName)
		_ = os.Remove(h.kubeconfig)
	})

	return nil
}

// installKeda installs KEDA; failure fails the harness. It goes away with the
// cluster, so no cleanup is registered.
func (h *e2eHarness) installKeda() error {
	_, err := execOut(h.repoRoot, h.kubeEnv(), "helm", "install", kedaRelease, kedaChartName,
		"--repo", kedaChartRepo,
		"--version", kedaChartVersion,
		"--namespace", kedaNamespace, "--create-namespace",
		"--wait", "--timeout", "5m",
	)

	return err
}

func (h *e2eHarness) loadControlImage() error {
	_, err := execOut(h.repoRoot, nil, "kind", "load", "docker-image",
		controlRepo+":"+imageVersion, "--name", clusterName)

	return err
}

// installSnapshotCRDs registers minimal VolumeSnapshot CRDs for the k8s-source test.
func (h *e2eHarness) installSnapshotCRDs() error {
	crds := filepath.Join("e2e", "testdata", "snapshot-crds.yaml")

	if _, err := execOut(h.repoRoot, h.kubeEnv(), "kubectl", "apply", "-f", crds); err != nil {
		return err
	}

	_, err := execOut(h.repoRoot, h.kubeEnv(), "kubectl", "wait", "--for", "condition=established",
		"--timeout", "60s",
		"crd/volumesnapshots.snapshot.storage.k8s.io",
		"crd/volumesnapshotcontents.snapshot.storage.k8s.io")

	return err
}

func (h *e2eHarness) joinTemporalToNetwork() error {
	out, err := execStdout(h.repoRoot, nil, "docker", "compose", "ps", "-q", temporalAlias)
	if err != nil {
		return err
	}

	h.temporalCID = strings.TrimSpace(out)
	if h.temporalCID == "" {
		return fmt.Errorf("no running Temporal container (run `make temporal-up`)")
	}

	if _, err := execOut(h.repoRoot, nil, "docker", "network", "connect",
		"--alias", temporalAlias, kindNetwork, h.temporalCID); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		return err
	}

	h.push(func() {
		_, _ = execOut(h.repoRoot, nil, "docker", "network", "disconnect", kindNetwork, h.temporalCID)
	})

	return nil
}

// cleanLeftoverWorkflows terminates Running backups left by a hard-killed prior
// run (the dev Temporal volume persists) so the fresh worker won't retry them.
func (h *e2eHarness) cleanLeftoverWorkflows() error {
	// Isolate envconfig from any host profile, matching dialTemporal.
	emptyConfig := filepath.Join(os.TempDir(), "tape-archiver-e2e-empty.toml")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		return err
	}

	_ = os.Setenv("TEMPORAL_CONFIG_FILE", emptyConfig)
	_ = os.Setenv("TEMPORAL_PROFILE", "")

	c, shutdown, err := temporalclient.New(context.Background(), nil)
	if err != nil {
		return err
	}
	defer shutdown()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Query: "ExecutionStatus='Running' AND WorkflowType='" + backup.WorkflowType + "'",
	})
	if err != nil {
		return err
	}

	for _, execution := range resp.GetExecutions() {
		id := execution.GetExecution().GetWorkflowId()
		if termErr := c.TerminateWorkflow(ctx, id, "", "e2e leftover cleanup"); termErr == nil {
			log.Printf("[e2e] terminated leftover workflow %s", id)
		}
	}

	return nil
}

func (h *e2eHarness) createStagingDir() error {
	// World-writable so both the root container and the test process can use it.
	dir, err := os.MkdirTemp("", "tape-archiver-e2e-staging-")
	if err != nil {
		return err
	}

	if err := os.Chmod(dir, 0o777); err != nil {
		return err
	}

	h.stagingHostDir = dir
	h.push(func() { _ = os.RemoveAll(dir) })

	return nil
}

// opticalDiscCount: two lets one burn set write both copies without a disc swap.
const opticalDiscCount = 2

// opticalDiscSize is ample for the tens-of-MB ISO; the backing file is sparse.
const opticalDiscSize = 512 << 20

// setupOpticalDiscs provisions loop devices as burners via xorriso's stdio
// pseudo-drive (there is no virtual optical writer). Best-effort: without
// losetup the optical tests skip.
func (h *e2eHarness) setupOpticalDiscs() error {
	if _, err := exec.LookPath("losetup"); err != nil {
		log.Printf("[e2e] optical-discs: losetup not on PATH; optical burn tests will skip")

		return nil
	}

	dir, err := os.MkdirTemp("", "tape-archiver-e2e-optical-")
	if err != nil {
		return err
	}

	h.push(func() { _ = os.RemoveAll(dir) })

	for i := 0; i < opticalDiscCount; i++ {
		backing := filepath.Join(dir, fmt.Sprintf("disc-%d.img", i))
		if err := os.WriteFile(backing, nil, 0o600); err != nil {
			return err
		}

		if err := os.Truncate(backing, opticalDiscSize); err != nil {
			return err
		}

		out, err := execStdout(h.repoRoot, nil, "losetup", "--find", "--show", backing)
		if err != nil {
			return fmt.Errorf("attach loop device for %s: %w", backing, err)
		}

		device := strings.TrimSpace(out)
		if device == "" {
			return fmt.Errorf("losetup returned no device for %s", backing)
		}

		h.opticalDevices = append(h.opticalDevices, device)
		h.opticalBackings = append(h.opticalBackings, backing)

		h.push(func() { _, _ = execOut(h.repoRoot, nil, "losetup", "--detach", device) })

		log.Printf("[e2e] optical-discs: %s backed by %s", device, backing)
	}

	return nil
}

func (h *e2eHarness) startWebhook() error {
	// The kind bridge gateway is the host's address on that network. The network
	// is dual-stack; take the IPv4 gateway.
	out, err := execStdout(h.repoRoot, nil, "docker", "network", "inspect", kindNetwork,
		"-f", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		return err
	}

	for _, gw := range strings.Fields(out) {
		if ip := net.ParseIP(gw); ip != nil && ip.To4() != nil {
			h.gatewayIP = gw

			break
		}
	}

	if h.gatewayIP == "" {
		return fmt.Errorf("could not determine an IPv4 kind network gateway (got %q)", strings.TrimSpace(out))
	}

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return err
	}

	port := listener.Addr().(*net.TCPAddr).Port
	h.webhookURL = fmt.Sprintf("http://%s:%d", h.gatewayIP, port)

	server := &http.Server{Handler: h.rec.mux()}

	go func() { _ = server.Serve(listener) }()

	h.push(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = server.Shutdown(ctx)
	})

	return nil
}

func (h *e2eHarness) deployControlWorker() error {
	chart := filepath.Join("deploy", "charts", "tape-archiver-control-worker")

	if _, err := execOut(h.repoRoot, nil, "helm", "dependency", "build", chart); err != nil {
		return err
	}

	imagePath := "resources.controllers.main.containers.main.image"

	if _, err := execOut(h.repoRoot, h.kubeEnv(), "helm", "install", helmRelease, chart,
		"--namespace", namespace, "--create-namespace", "--wait", "--timeout", "4m",
		"--set", "config.temporal.address="+temporalAlias+":"+temporalPort,
		"--set", "config.controlWorker.discordFailureWebhookUrl.value="+h.failureURL(),
		"--set", "config.controlWorker.k8sDatasetParent="+datasetParent,
		"--set-string", imagePath+".repository="+controlRepo,
		"--set-string", imagePath+".tag="+imageVersion,
		"--set", imagePath+".pullPolicy=Never",
	); err != nil {
		return err
	}

	h.push(func() {
		_, _ = execOut(h.repoRoot, h.kubeEnv(), "helm", "uninstall", helmRelease,
			"--namespace", namespace, "--wait", "--timeout", "2m")
	})

	return nil
}

// installScaledJobWorker installs the KEDA ScaledJob-shaped control worker with a
// fast scale-to-zero cycle. No keda.apiKey is set: KEDA's Temporal scaler forces
// TLS whenever one is present, and dev Temporal is plaintext. Callers must first
// scale the shared Deployment to 0 so scale-from-zero is observable.
func (h *e2eHarness) installScaledJobWorker(t *testing.T) {
	t.Helper()

	chart := filepath.Join("deploy", "charts", "tape-archiver-control-worker")

	_, err := execOut(h.repoRoot, nil, "helm", "dependency", "build", chart)
	require.NoError(t, err, "helm dependency build")

	imagePath := "resources.controllers.main.containers.main.image"

	// Registered before install so a partial install can't leave a stray
	// ScaledJob spawning pollers into later tests.
	t.Cleanup(func() { h.removeScaledJobWorker(t) })

	// No --wait: helm's readiness wait trips on the ScaledJob's transient
	// "InProgress" status, and no Job exists until there is a backlog.
	_, err = execOut(h.repoRoot, h.kubeEnv(), "helm", "install", scaledJobRelease, chart,
		"--namespace", namespace,
		"--set", "config.temporal.address="+temporalAlias+":"+temporalPort,
		"--set", "config.controlWorker.discordFailureWebhookUrl.value="+h.failureURL(),
		"--set", "resources.controllers.main.type=scaledjob",
		"--set", "resources.controllers.main.keda.pollingInterval=5",
		"--set-string", "resources.controllers.main.containers.main.env.WORKER_IDLE_EXIT_AFTER=20s",
		"--set-string", imagePath+".repository="+controlRepo,
		"--set-string", imagePath+".tag="+imageVersion,
		"--set", imagePath+".pullPolicy=Never",
	)
	require.NoError(t, err, "helm install scaledjob worker")
}

// removeScaledJobWorker tears the ScaledJob release down and blocks until none
// of its Jobs or pods remain (#424). KEDA's Jobs are garbage-collected in the
// background once the ScaledJob goes, so a bare `helm uninstall --wait` can
// return while a worker pod still polls the shared control queue and runs later
// tests' activities with its deleted ServiceAccount's token. Removing the
// ScaledJob and Jobs before the release also keeps that token valid until the
// pods are gone.
func (h *e2eHarness) removeScaledJobWorker(t *testing.T) {
	t.Helper()

	selector := "app.kubernetes.io/instance=" + scaledJobRelease

	// Stop KEDA spawning Jobs before removing the existing ones.
	_, err := execOut(h.repoRoot, h.kubeEnv(), "kubectl", "delete", "scaledjobs.keda.sh",
		"-n", namespace, "-l", selector, "--wait", "--timeout=2m", "--ignore-not-found")
	assert.NoError(t, err, "delete the autoscaling ScaledJob")

	// Foreground: a Job is only gone once its pods are.
	_, err = execOut(h.repoRoot, h.kubeEnv(), "kubectl", "delete", "jobs",
		"-n", namespace, "-l", selector, "--cascade=foreground", "--wait", "--timeout=3m", "--ignore-not-found")
	assert.NoError(t, err, "delete the autoscaling ScaledJob's Jobs")

	assert.Eventuallyf(t, func() bool {
		out, listErr := execStdout(h.repoRoot, h.kubeEnv(), "kubectl", "get", "pods",
			"-n", namespace, "-l", selector, "-o", "name")

		return listErr == nil && strings.TrimSpace(out) == ""
	}, 3*time.Minute, 2*time.Second, "no %s pod may outlive the autoscaling test to poll later tests' control queue", scaledJobRelease)

	_, err = execOut(h.repoRoot, h.kubeEnv(), "helm", "uninstall", scaledJobRelease,
		"--namespace", namespace, "--wait", "--timeout", "2m", "--ignore-not-found")
	assert.NoError(t, err, "uninstall the autoscaling release")
}

// scaleControlDeployment scales the shared control Deployment and waits for it to settle.
func (h *e2eHarness) scaleControlDeployment(t *testing.T, replicas int) {
	t.Helper()

	_, err := execOut(h.repoRoot, h.kubeEnv(), "kubectl", "scale", "deployment",
		"-n", namespace, "-l", "app.kubernetes.io/instance="+helmRelease,
		fmt.Sprintf("--replicas=%d", replicas))
	require.NoErrorf(t, err, "scale control deployment to %d", replicas)

	require.Eventuallyf(t, func() bool {
		return h.runningControlPods(t, helmRelease) == replicas
	}, 2*time.Minute, 2*time.Second, "control deployment must settle at %d running pod(s)", replicas)
}

// restoreControlReplicas best-effort restores the shared Deployment to one replica.
func (h *e2eHarness) restoreControlReplicas() {
	_, _ = execOut(h.repoRoot, h.kubeEnv(), "kubectl", "scale", "deployment",
		"-n", namespace, "-l", "app.kubernetes.io/instance="+helmRelease, "--replicas=1")
}

// runningControlPods counts Running pods only, so a self-exited KEDA Job pod
// reads as scaled to zero.
func (h *e2eHarness) runningControlPods(t *testing.T, release string) int {
	t.Helper()

	out, err := execStdout(h.repoRoot, h.kubeEnv(), "kubectl", "get", "pods",
		"-n", namespace, "-l", "app.kubernetes.io/instance="+release,
		"--field-selector=status.phase=Running", "-o", "name")
	require.NoErrorf(t, err, "list running control pods for %s", release)

	return len(strings.Fields(out))
}

func (h *e2eHarness) startDataWorker() error {
	// Trigger the snapshot automount and make it propagate into the container.
	snapshot := os.Getenv("TAPE_TEST_SNAPSHOT")
	_, _ = execOut(h.repoRoot, nil, "sh", "-c",
		fmt.Sprintf("ls %s/archive/.zfs/snapshot/%s >/dev/null 2>&1", poolMount, snapshot))
	_, _ = execOut(h.repoRoot, nil, "sudo", "mount", "--make-rshared", poolMount)

	args := []string{"run", "-d", "--name", "tape-archiver-e2e-data", "--network", kindNetwork}

	devices := []string{
		envOr("MHVTL_CHANGER_DEV", "/dev/sch0"),
		envOr("MHVTL_DRIVE0_DEV", "/dev/nst0"), envOr("MHVTL_DRIVE1_DEV", "/dev/nst1"),
		"/dev/sg0", "/dev/sg1", "/dev/sg2", "/dev/fuse", "/dev/zfs",
	}

	devices = append(devices, h.opticalDevices...)
	if realBurner := os.Getenv(testutil.OpticalBurnDevEnv); realBurner != "" {
		devices = append(devices, realBurner)
	}

	for _, dev := range devices {
		if _, err := os.Stat(dev); err == nil {
			args = append(args, "--device", dev)
		}
	}

	args = append(args,
		"--cap-add", "SYS_ADMIN", // LTFS is FUSE-based and cannot mount without it
		"--cap-add", "SYS_RAWIO", // changer / sg_raw / LTFS issue raw SCSI via SG_IO
		"-v", poolMount+":"+poolMount+":rshared", // ZFS source + .zfs/snapshot automounts
		"-v", h.stagingHostDir+":"+containerStagingDir, // staged archives + PAR2 (ample host dir)
		// The recovery set is baked into the image, so it needs no mount.
		"-e", "ROLE=data",
		"-e", "TEMPORAL_ADDRESS="+temporalAlias+":"+temporalPort,
		"-e", "TEMPORAL_NAMESPACE="+namespace,
		"-e", "TAPE_STAGING_DIR="+containerStagingDir,
		dataRepo+":"+imageVersion,
	)

	out, err := execStdout(h.repoRoot, nil, "docker", args...)
	if err != nil {
		return err
	}

	h.dataCID = strings.TrimSpace(out)
	h.push(func() {
		logs, _ := execOut(h.repoRoot, nil, "docker", "logs", h.dataCID)
		log.Printf("[e2e] data-worker container logs:\n%s", logs)

		_, _ = execOut(h.repoRoot, nil, "docker", "rm", "-f", h.dataCID)
	})

	return h.waitForDataWorker(90 * time.Second)
}

func (h *e2eHarness) waitForDataWorker(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		logs, _ := execOut(h.repoRoot, nil, "docker", "logs", h.dataCID)
		if strings.Contains(logs, "starting worker") {
			return nil
		}

		if state, _ := execStdout(h.repoRoot, nil, "docker", "inspect", "-f", "{{.State.Running}}", h.dataCID); strings.TrimSpace(state) != "true" {
			return fmt.Errorf("data worker container exited during startup:\n%s", logs)
		}

		time.Sleep(2 * time.Second)
	}

	logs, _ := execOut(h.repoRoot, nil, "docker", "logs", h.dataCID)

	return fmt.Errorf("data worker did not start within %s:\n%s", timeout, logs)
}

// push registers a teardown action, run LIFO by teardown.
func (h *e2eHarness) push(fn func()) { h.cleanups = append(h.cleanups, fn) }

func (h *e2eHarness) teardown() {
	for i := len(h.cleanups) - 1; i >= 0; i-- {
		h.cleanups[i]()
	}
}

func (h *e2eHarness) kubeEnv() []string { return []string{"KUBECONFIG=" + h.kubeconfig} }

// deliveryURL is the per-run success-delivery webhook URL.
func (h *e2eHarness) deliveryURL(runID string) string {
	return h.webhookURL + "/delivery/" + runID
}

// failureURL is the failure-alert webhook baked into the control worker at install.
func (h *e2eHarness) failureURL() string { return h.webhookURL + "/failure" }

// upload is one delivered artifact captured by the mock webhook.
type upload struct {
	filename string
	data     []byte
}

// recorder captures mock webhook uploads and failure alerts.
type recorder struct {
	mu         sync.Mutex
	deliveries map[string][]upload
	failures   []string
}

func newRecorder() *recorder {
	return &recorder{deliveries: make(map[string][]upload)}
}

func (r *recorder) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/delivery/", r.handleDelivery)
	mux.HandleFunc("/failure", r.handleFailure)

	return mux
}

func (r *recorder) handleDelivery(w http.ResponseWriter, req *http.Request) {
	runID := strings.TrimPrefix(req.URL.Path, "/delivery/")

	file, header, err := req.FormFile("files[0]")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	r.mu.Lock()
	r.deliveries[runID] = append(r.deliveries[runID], upload{filename: header.Filename, data: data})
	r.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (r *recorder) handleFailure(w http.ResponseWriter, req *http.Request) {
	var msg struct {
		Content string `json:"content"`
	}

	if err := json.NewDecoder(req.Body).Decode(&msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	r.mu.Lock()
	r.failures = append(r.failures, msg.Content)
	r.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (r *recorder) uploadsFor(runID string) []upload {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]upload(nil), r.deliveries[runID]...)
}

func (r *recorder) failureMessages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.failures...)
}

// ---- shared test helpers ----------------------------------------------------

// dialTemporal connects to TEMPORAL_ADDRESS, isolated from host envconfig.
func dialTemporal(t *testing.T) client.Client {
	t.Helper()

	emptyConfig := filepath.Join(t.TempDir(), "empty.toml")
	require.NoError(t, os.WriteFile(emptyConfig, nil, 0o600))
	t.Setenv("TEMPORAL_CONFIG_FILE", emptyConfig)
	t.Setenv("TEMPORAL_PROFILE", "")

	temporalClient, shutdown, err := temporalclient.New(t.Context(), nil)
	require.NoError(t, err, "connect to Temporal")
	t.Cleanup(shutdown)

	return temporalClient
}

// generateTestKeypair returns a post-quantum age identity file and its recipient.
func generateTestKeypair(t *testing.T) (identity, recipient string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "identity.txt")
	require.NoError(t, exec.CommandContext(t.Context(), "age-keygen", "-pq", "-o", path).Run(), "age-keygen")

	contents, err := os.ReadFile(path)
	require.NoError(t, err)

	const marker = "# public key: "

	for _, line := range strings.Split(string(contents), "\n") {
		if after, found := strings.CutPrefix(line, marker); found {
			recipient = strings.TrimSpace(after)

			break
		}
	}

	require.NotEmpty(t, recipient, "recipient not found in identity file")

	return string(contents), recipient
}

// backupWorkflowID is the singleton run ID (SPEC §4.2), mirroring cmd/tapectl's
// unexported constant. The per-test runID is only a webhook bucket token.
const backupWorkflowID = "backup"

// submitRun submits cfg via `tapectl run --config`, retrying while a prior
// test's singleton run is still closing.
func (h *e2eHarness) submitRun(t *testing.T, cfg config.Config) {
	t.Helper()

	data, err := json.Marshal(cfg)
	require.NoError(t, err, "marshal run config")

	configPath := filepath.Join(t.TempDir(), "run-config.json")
	require.NoError(t, os.WriteFile(configPath, data, 0o600))

	deadline := time.Now().Add(90 * time.Second)

	for {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		out, err := exec.CommandContext(ctx, h.tapectlPath, "run", "--config", configPath).CombinedOutput()

		cancel()

		if err == nil {
			require.Equal(t, backupWorkflowID, strings.TrimSpace(string(out)),
				"tapectl must echo the singleton workflow ID")

			return
		}

		// Also retry transient dial failures to the shared dev Temporal.
		retryable := strings.Contains(string(out), "already in progress") ||
			strings.Contains(string(out), "connect to Temporal") ||
			strings.Contains(string(out), "dial Temporal")
		if retryable && time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)

			continue
		}

		require.NoErrorf(t, err, "tapectl run: %s", out)
	}
}

// resumeRun resumes the paused run via `tapectl resume` (issue #67).
func (h *e2eHarness) resumeRun(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, h.tapectlPath, "resume").CombinedOutput()
	require.NoErrorf(t, err, "tapectl resume: %s", out)
}

// temporalRunID returns the current RunID, which keys the staging directory.
func temporalRunID(t *testing.T, c client.Client) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	description, err := c.DescribeWorkflowExecution(ctx, backupWorkflowID, "")
	require.NoError(t, err, "describe workflow %s", backupWorkflowID)

	return description.GetWorkflowExecutionInfo().GetExecution().GetRunId()
}

func ptrFloat(f float64) *float64 { return &f }

// terminateOnCleanup terminates the run at test end so the singleton ID is free
// for the next test.
func terminateOnCleanup(t *testing.T, c client.Client) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 15*time.Second)
		defer cancel()

		_ = c.TerminateWorkflow(ctx, backupWorkflowID, "", "e2e cleanup")
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

// execOut returns combined output; the error includes it.
func execOut(dir string, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}

	cmd.Env = append(os.Environ(), extraEnv...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}

	return string(out), nil
}

// execStdout returns stdout only, since nix/docker warnings on stderr would
// corrupt parsed output; the error carries stderr.
func execStdout(dir string, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}

	cmd.Env = append(os.Environ(), extraEnv...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, stderr.String())
	}

	return string(out), nil
}

// probeDriveReadiness loads, readies and unloads drive 0 once. A drive that never
// readies is a skip reason; later per-test waits fail instead, so a mid-suite
// wedge can't hide behind "ok".
func probeDriveReadiness() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	changer := tape.NewChanger(envOr("MHVTL_CHANGER_DEV", "/dev/sch0"))

	inv, err := changer.Inventory(ctx)
	if err != nil {
		return "", fmt.Errorf("changer inventory: %w", err)
	}

	if len(inv.Drives) == 0 {
		return "", fmt.Errorf("changer reports no drives")
	}

	if inv.Drives[0].Loaded {
		return "", fmt.Errorf("drive 0 must start empty (run `make mhvtl-reset`)")
	}

	slotAddr := -1

	for _, slot := range inv.Slots {
		if slot.Full {
			slotAddr = slot.Address

			break
		}
	}

	if slotAddr == -1 {
		return "", fmt.Errorf("no storage slot holds a tape")
	}

	driveAddr := inv.Drives[0].Address
	if err := changer.Load(ctx, slotAddr, driveAddr); err != nil {
		return "", fmt.Errorf("load slot %d into drive 0: %w", slotAddr, err)
	}

	readyErr := testutil.WaitDriveReady(ctx, testutil.ResolveDrive0Dev())

	if err := changer.Unload(ctx, slotAddr, driveAddr); err != nil {
		return "", fmt.Errorf("unload drive 0 to slot %d: %w", slotAddr, err)
	}

	if readyErr != nil {
		return fmt.Sprintf("%v (kernel/mhvtl ioctl incompatibility)", readyErr), nil
	}

	return "", nil
}
