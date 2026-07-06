//go:build e2e

// Package e2e is the end-to-end test suite (issue #20). It drives the whole
// backup workflow the way it ships in production: the control worker deployed
// via its Helm chart + OCI image into a real kind cluster, the data worker run
// as its OCI container on the host with the tape devices and ZFS pool, a real
// dev Temporal joined to the kind network, and mhvtl standing in for the tape
// library. It consumes only the exported workflows/backup API plus the public
// pkg/* + internal/testutil helpers, so it is a genuine black-box, whole-system
// test.
//
// Topology (confirmed with the maintainer):
//
//	kind cluster (docker network "kind")
//	 ├─ control-worker Deployment  ← Helm chart, control-worker image
//	 │     ROLE=control → temporal:7233
//	 └─ temporal container (joined to the kind network, alias "temporal")
//	host:
//	 ├─ data-worker container  --network kind, tape-device + /dev/zfs passthrough,
//	 │     pool bind mount, recovery-binaries mount → temporal:7233
//	 └─ this test process: blank-tape prep, run submission, and one shared mock
//	      webhook (reachable from the cluster via the kind bridge gateway IP)
//
// The whole harness is guarded so it skips cleanly (never fails) when any
// prerequisite — docker/kind/helm/nix, Temporal, mhvtl, LTFS, or the ZFS pool —
// is absent (issue #20 AC5). Driven by `make test-e2e`.
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

	"github.com/stretchr/testify/require"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
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

	// temporalAlias is the network alias the Temporal container gets on the kind
	// network so the in-cluster control worker and the data container both resolve
	// it at a stable "temporal:7233" regardless of the compose container name.
	temporalAlias = "temporal"
	temporalPort  = "7233"

	// poolMount is the on-host ZFS pool root the data container bind-mounts. Both
	// the dataset mount (…/archive) and its on-demand .zfs/snapshot/ automounts
	// live under it; rshared propagation carries them into the container.
	poolMount = "/mnt/tape-test-pool"
	// containerStagingDir is where the host staging directory is bind-mounted in
	// the data container (TAPE_STAGING_DIR). Staging must NOT live in the small
	// ephemeral ZFS test pool (it holds only the source payload); it gets its own
	// ample host directory, bind-mounted here and readable by the host test
	// process for the AC4 slice corruption.
	containerStagingDir = "/staging"
	// containerRecoveryBin is where the static recovery-binary set is mounted in
	// the data container (TAPE_RECOVERY_BINARIES_DIR).
	containerRecoveryBin = "/recovery-bin"

	// datasetParent is democratic-csi's datasetParentName for the k8s-source test:
	// the CSI snapshotHandle's volume component is joined under it to rebuild the
	// absolute ZFS path. It is the test pool root, so `<parent>/pvc-<uuid>` is a
	// child dataset of the pool. Wired into the control worker as K8sDatasetParent.
	datasetParent = "tape_test"

	// scaledJobRelease is the separate Helm release used by the autoscaling tests to
	// install the control worker in its KEDA ScaledJob shape, alongside (not on top
	// of) the shared Deployment release. It is installed and uninstalled by the
	// autoscaling test itself, not the shared harness.
	scaledJobRelease = "ta-control-sj"

	// kedaNamespace / kedaRelease / kedaChartVersion pin the KEDA operator install.
	// The built-in Temporal scaler the control-worker ScaledJob depends on ships with
	// KEDA from 2.17 onward (it does not exist in 2.16). KEDA is a hard prerequisite of
	// the suite: installKeda fails the whole harness if the install fails, so the
	// autoscaling path is genuinely covered rather than silently skipped.
	kedaNamespace    = "keda"
	kedaRelease      = "keda"
	kedaChartName    = "keda"
	kedaChartVersion = "2.18.3"
	// kedaChartRepo is the KEDA chart's GitHub Pages Helm repository, used via
	// helm's --repo flag so no global repo entry is added to the host. (KEDA also
	// publishes an OCI chart at ghcr.io, but the Pages repo is the more broadly
	// reachable of the two; the operator images still come from ghcr regardless.)
	kedaChartRepo = "https://kedacore.github.io/charts"
	// rbacName is the ClusterRole + binding granting the control worker's
	// ServiceAccount read access to VolumeSnapshots (the chart omits this by
	// design — it is the operator's responsibility).
	rbacName = "tape-archiver-e2e-snapshot-reader"
)

// orderedPhases is the full phase sequence the backup workflow completes, in order
// (SPEC §4.3). Built from the exported constants so it tracks any rename. The Burn
// phase always runs — it is a no-op that still completes (and is recorded) when
// optical burning is disabled — so it appears here between Report and Deliver for
// every run, burning or not.
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
	// harness is the shared, running system under test, populated by TestMain when
	// every prerequisite is present.
	harness *e2eHarness
	// skipReason is set (and harness left nil) when a prerequisite is missing, so
	// every test skips cleanly instead of failing (AC5).
	skipReason string
)

// e2eHarness owns the running cluster, containers, and mock webhook for the
// suite, plus the LIFO cleanup stack that tears them all down.
type e2eHarness struct {
	repoRoot       string
	kubeconfig     string
	tapectlPath    string // built tapectl binary used to submit runs (the operator path)
	recoveryBinDir string // host path to the static recovery binaries (…/bin)
	stagingHostDir string // host staging dir bind-mounted into the data container
	temporalCID    string
	dataCID        string
	gatewayIP      string // host address reachable from the kind network
	webhookURL     string // base URL, e.g. http://172.18.0.1:39000
	rec            *recorder

	// opticalDevices are loop-device-backed pseudo-burners for the optical burn
	// tests, created host-side and passed through to the data-worker container.
	// Empty when losetup is unavailable, so the optical tests skip and the rest of
	// the suite is unaffected. opticalBackings holds their backing files (detached
	// on teardown alongside the loop devices).
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

// requireHarness returns the running harness, or skips the calling test when a
// prerequisite was missing (AC5).
func requireHarness(t *testing.T) *e2eHarness {
	t.Helper()

	if harness == nil {
		t.Skipf("e2e prerequisites unavailable: %s", skipReason)
	}

	return harness
}

// checkPrerequisites returns a human-readable reason to skip, or "" when the
// environment can run the suite. It mirrors the device/tool checks the
// testutil.SkipIf* helpers make, but returns a reason instead of needing a
// *testing.T (unavailable in TestMain).
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

// setupHarness brings the whole system up, registering a teardown step after
// each resource is created so a mid-setup failure still unwinds cleanly.
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
		{"recovery-binaries", h.buildRecoveryBinaries},
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
		{"snapshot-rbac", h.grantSnapshotRBAC},
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
	// nix caches aggressively, so this is near-instant once the images exist.
	// VERSION=e2e gives the images stable tags the cluster load + chart override +
	// data container all reference.
	_, err := execOut(h.repoRoot, nil, "make", "build-images", "VERSION="+imageVersion)

	return err
}

// buildTapectl builds the tapectl CLI once; the tests submit runs through it (the
// real operator path) rather than calling the Temporal API directly.
func (h *e2eHarness) buildTapectl() error {
	h.tapectlPath = filepath.Join(os.TempDir(), "tape-archiver-e2e-tapectl")

	_, err := execOut(h.repoRoot, nil, "go", "build", "-o", h.tapectlPath, "./cmd/tapectl")
	if err != nil {
		return err
	}

	h.push(func() { _ = os.Remove(h.tapectlPath) })

	return nil
}

func (h *e2eHarness) buildRecoveryBinaries() error {
	out, err := execStdout(h.repoRoot, nil, "nix", "build", "--no-link", "--print-out-paths", ".#recoveryBinaries")
	if err != nil {
		return err
	}

	h.recoveryBinDir = filepath.Join(strings.TrimSpace(out), "bin")
	if _, statErr := os.Stat(filepath.Join(h.recoveryBinDir, "age")); statErr != nil {
		return fmt.Errorf("recovery binaries missing at %s: %w", h.recoveryBinDir, statErr)
	}

	return nil
}

func (h *e2eHarness) createCluster() error {
	h.kubeconfig = filepath.Join(os.TempDir(), "tape-archiver-e2e.kubeconfig")

	// Delete any leftover cluster from an interrupted run so create starts clean.
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

// installKeda installs the KEDA operator (with its CRDs, including ScaledJob and
// TriggerAuthentication) into the kind cluster via its published OCI Helm chart. The
// control-worker ScaledJob path and its Temporal scaler are KEDA features, so KEDA is
// a hard prerequisite of the suite: a failed install fails the whole harness rather
// than skipping the autoscaling tests, so the scale-to-zero path is genuinely covered.
// KEDA is torn down with the cluster (createCluster's delete), so no separate cleanup
// is registered.
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

// installSnapshotCRDs registers the minimal VolumeSnapshot/VolumeSnapshotContent
// CRDs so the k8s-source test can create those objects and the control worker can
// read them.
func (h *e2eHarness) installSnapshotCRDs() error {
	crds := filepath.Join("e2e", "testdata", "snapshot-crds.yaml")

	if _, err := execOut(h.repoRoot, h.kubeEnv(), "kubectl", "apply", "-f", crds); err != nil {
		return err
	}

	// Wait until the API server serves the new kinds before any CR is created.
	_, err := execOut(h.repoRoot, h.kubeEnv(), "kubectl", "wait", "--for", "condition=established",
		"--timeout", "60s",
		"crd/volumesnapshots.snapshot.storage.k8s.io",
		"crd/volumesnapshotcontents.snapshot.storage.k8s.io")

	return err
}

// grantSnapshotRBAC binds a ClusterRole (get/list on VolumeSnapshots +
// VolumeSnapshotContents) to the control worker's ServiceAccount. The chart omits
// this by design (operator's responsibility), so the k8s-source path would fail
// with a forbidden error without it — the test exercises that requirement too.
func (h *e2eHarness) grantSnapshotRBAC() error {
	sa, err := execStdout(h.repoRoot, h.kubeEnv(), "kubectl", "get", "deployment", "-n", namespace,
		"-o", "jsonpath={.items[0].spec.template.spec.serviceAccountName}")
	if err != nil {
		return err
	}

	sa = strings.TrimSpace(sa)
	if sa == "" {
		sa = helmRelease
	}

	if _, err := execOut(h.repoRoot, h.kubeEnv(), "kubectl", "create", "clusterrole", rbacName,
		"--verb=get,list",
		"--resource=volumesnapshots.snapshot.storage.k8s.io,volumesnapshotcontents.snapshot.storage.k8s.io",
	); err != nil {
		return err
	}

	if _, err := execOut(h.repoRoot, h.kubeEnv(), "kubectl", "create", "clusterrolebinding", rbacName,
		"--clusterrole="+rbacName, "--serviceaccount="+namespace+":"+sa,
	); err != nil {
		return err
	}

	h.push(func() {
		_, _ = execOut(h.repoRoot, h.kubeEnv(), "kubectl", "delete", "clusterrolebinding", rbacName, "--ignore-not-found")
		_, _ = execOut(h.repoRoot, h.kubeEnv(), "kubectl", "delete", "clusterrole", rbacName, "--ignore-not-found")
	})

	return nil
}

func (h *e2eHarness) joinTemporalToNetwork() error {
	// Discover the compose-managed Temporal container regardless of its
	// project-derived name.
	out, err := execStdout(h.repoRoot, nil, "docker", "compose", "ps", "-q", temporalAlias)
	if err != nil {
		return err
	}

	h.temporalCID = strings.TrimSpace(out)
	if h.temporalCID == "" {
		return fmt.Errorf("no running Temporal container (run `make temporal-up`)")
	}

	// Idempotent: connecting an already-connected container errors; ignore that.
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

// cleanLeftoverWorkflows terminates any still-Running Backup workflows before the
// run. Temporal's dev volume persists across restarts, so a prior hard-killed run
// can leave open workflows whose stale activities the fresh data worker would pick
// up and retry — noise that wastes the worker and muddies diagnosis. Best-effort.
func (h *e2eHarness) cleanLeftoverWorkflows() error {
	// Isolate the Temporal envconfig from any stray host profile, matching
	// dialTemporal; the address comes from TEMPORAL_ADDRESS.
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
	// A generous host directory (not the tiny ZFS test pool) for the Prepare
	// phase's staged archives + PAR2 sets. World-writable so both the container
	// (root) and the host test process can access it.
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

// opticalDiscCount is how many loop-device pseudo-burners the harness provisions
// for the optical burn tests. Two lets a single burn-set drive two copies in
// parallel (TestBackupEndToEnd_OpticalBurn) with no operator disc-swap pause, and
// the reclaim test reuses the first.
const opticalDiscCount = 2

// opticalDiscSize is the backing size of each loop-device pseudo-disc. The recovery
// ISO is tens of MB (report, manifest, a handful of static recovery binaries); half
// a gibibyte is ample while staying a sparse file that costs nothing until written.
const opticalDiscSize = 512 << 20

// setupOpticalDiscs provisions loop-device-backed pseudo-burners for the optical
// burn tests and passes them into the data-worker container (startDataWorker). It
// is the optical analogue of the mhvtl library: there is no faithful virtual
// optical writer, so a loop device driven through xorriso's stdio pseudo-drive
// stands in for a burner (the same mechanism pkg/optical and the burn integration
// tests use). It is best-effort: when losetup is unavailable it logs and leaves the
// device list empty, so the optical tests skip (requireOpticalDiscs) and the rest
// of the suite is unaffected — make test-e2e stays green without optical support.
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
	// The gateway of the kind bridge is the host's address on that network, i.e.
	// the address in-cluster pods and the data container reach the host webhook at.
	// The kind network is dual-stack; list every gateway and take the IPv4 one (the
	// webhook listens on 0.0.0.0, and an IPv6 literal would also need bracketing).
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

	// Bind all interfaces so the docker bridge can reach it; advertise the gateway
	// IP to the workers.
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

	// Vendor the bjw-s common dependency from the committed Chart.lock (offline).
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

// installScaledJobWorker installs the control worker in its KEDA ScaledJob shape as a
// separate release (scaledJobRelease), alongside — not replacing — the shared
// Deployment release. The overlay drives a fast scale-to-zero cycle observable in
// seconds: a low KEDA pollingInterval and a short WORKER_IDLE_EXIT_AFTER so the Job
// self-completes quickly once the control queue goes idle. No config.temporal.keda.apiKey
// is set: the dev Temporal is plaintext and unauthenticated, and KEDA's Temporal scaler
// forces TLS whenever an API key is present — so an anonymous (no-apiKey) connection is
// the only way it reaches this frontend. It registers a t.Cleanup that uninstalls the
// release (which cascades KEDA's spawned Jobs via their ownerReferences). Callers must
// first remove the shared Deployment worker as a poller (scaleControlDeployment 0), so the
// KEDA-spawned worker is the only one on the control queue and scale-from-zero is observable.
func (h *e2eHarness) installScaledJobWorker(t *testing.T) {
	t.Helper()

	chart := filepath.Join("deploy", "charts", "tape-archiver-control-worker")

	// Vendor the bjw-s common dependency from the committed Chart.lock (offline);
	// idempotent with deployControlWorker's earlier build.
	_, err := execOut(h.repoRoot, nil, "helm", "dependency", "build", chart)
	require.NoError(t, err, "helm dependency build")

	imagePath := "resources.controllers.main.containers.main.image"

	// Register the uninstall before installing so a failed/partial install (e.g. a
	// left-behind Secret or a ScaledJob KEDA has started acting on) is still cleaned
	// up — otherwise a stray ScaledJob would keep spawning control-queue pollers that
	// interfere with every later test in the shared suite.
	t.Cleanup(func() {
		_, _ = execOut(h.repoRoot, h.kubeEnv(), "helm", "uninstall", scaledJobRelease,
			"--namespace", namespace, "--wait", "--timeout", "2m", "--ignore-not-found")
	})

	// No --wait: the only workload this release yields is a KEDA-spawned Job that does
	// not exist until a backlog appears, and helm's readiness wait trips on the
	// ScaledJob's transient "InProgress" status. The scale assertions poll pods
	// directly instead.
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

// scaleControlDeployment scales the shared Deployment control worker (helmRelease) to
// the given replica count and waits for the rollout to settle there. The autoscaling
// tests scale it to 0 so the KEDA-spawned worker is the sole control-queue poller; the
// multi-replica test scales it to 2 to run two concurrent pollers.
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

// restoreControlReplicas scales the shared Deployment control worker back to its
// harness default of one replica. Best-effort (no *testing.T), for use in a cleanup so
// a later test still finds the baseline Deployment serving the control queue.
func (h *e2eHarness) restoreControlReplicas() {
	_, _ = execOut(h.repoRoot, h.kubeEnv(), "kubectl", "scale", "deployment",
		"-n", namespace, "-l", "app.kubernetes.io/instance="+helmRelease, "--replicas=1")
}

// runningControlPods returns the number of Running-phase pods for the given control
// worker release. It counts only Running pods, so a KEDA Job pod that has Completed
// (Succeeded phase) after a self-exit reads as scaled-to-zero, which is exactly the
// "no worker running" signal the autoscaling assertions turn on.
func (h *e2eHarness) runningControlPods(t *testing.T, release string) int {
	t.Helper()

	out, err := execStdout(h.repoRoot, h.kubeEnv(), "kubectl", "get", "pods",
		"-n", namespace, "-l", "app.kubernetes.io/instance="+release,
		"--field-selector=status.phase=Running", "-o", "name")
	require.NoErrorf(t, err, "list running control pods for %s", release)

	return len(strings.Fields(out))
}

func (h *e2eHarness) startDataWorker() error {
	// Ensure the snapshot is automounted on the host and the pool subtree is
	// rshared so the .zfs/snapshot/ automounts propagate into the container.
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

	// The optical burn tests burn to these inside the data worker: the loop-device
	// pseudo-burners provisioned by setupOpticalDiscs, plus a real burner named by
	// OPTICAL_BURN_DEV for the opt-in real-hardware test (when present).
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
		"-v", h.recoveryBinDir+":"+containerRecoveryBin+":ro",
		"-e", "ROLE=data",
		"-e", "TEMPORAL_ADDRESS="+temporalAlias+":"+temporalPort,
		"-e", "TEMPORAL_NAMESPACE="+namespace,
		"-e", "TAPE_STAGING_DIR="+containerStagingDir,
		"-e", "TAPE_RECOVERY_BINARIES_DIR="+containerRecoveryBin,
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

	// Wait until the worker has started polling the data queue (its startup slog
	// line), so the run does not stall waiting for a poller.
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

// deliveryURL is the per-run success-delivery webhook URL (report + ISO uploads),
// keyed by run ID so concurrent-safe bucketing survives even though tests are
// serialized.
func (h *e2eHarness) deliveryURL(runID string) string {
	return h.webhookURL + "/delivery/" + runID
}

// failureURL is the fixed failure-alert webhook baked into the control worker
// Deployment at install time.
func (h *e2eHarness) failureURL() string { return h.webhookURL + "/failure" }

// upload is one delivered artifact captured by the mock webhook.
type upload struct {
	filename string
	data     []byte
}

// recorder captures the mock webhook traffic: per-run artifact uploads and
// failure alerts.
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

// dialTemporal connects the test process to the host Temporal (TEMPORAL_ADDRESS),
// isolating envconfig from any stray host config, and registers client shutdown.
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

// generateTestKeypair produces an age post-quantum keypair, returning the full
// identity file contents (the AGE-SECRET-KEY escrow material) and its recipient.
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

// backupWorkflowID is the fixed, singleton workflow ID every backup run submits
// under (SPEC §4.2 — the model is serial: one data worker, one storage host, one
// staging area). It mirrors the same constant in cmd/tapectl (package main, so not
// importable). Runs are mutually exclusive, so the suite (which runs serially,
// -p 1) submits under this one ID and awaits/terminates it by name; the per-test
// runID lives on only as the webhook-delivery bucket token in the run config URL.
const backupWorkflowID = "backup"

// submitRun submits a backup run through the tapectl CLI — the operator path — by
// writing the config as JSON and invoking `tapectl run --config <file>`. It
// inherits the process env (TEMPORAL_ADDRESS + the dialTemporal isolation) and
// asserts the CLI echoes back the singleton workflow ID. Because the workflow ID
// is a singleton, a run left over from a prior test may still be closing; the
// submit is retried until that run's ID frees (tapectl reports a conflict as "a
// backup run is already in progress"), mirroring the tapectl integration test.
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

		// Retry a run left over from a prior test whose singleton ID is still closing,
		// and a transient failure to reach the shared dev Temporal (a brief dial
		// timeout under load is not a real submit failure).
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

// resumeRun resumes the paused backup run by invoking `tapectl resume`, exercising
// the operator CLI path end to end (issue #67). Runs are a singleton, so resume
// takes no arguments and acts on backupWorkflowID.
func (h *e2eHarness) resumeRun(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, h.tapectlPath, "resume").CombinedOutput()
	require.NoErrorf(t, err, "tapectl resume: %s", out)
}

// temporalRunID returns the Temporal RunID of the current backup run. The staging
// directory is keyed by this, not the (singleton) workflow ID.
func temporalRunID(t *testing.T, c client.Client) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	description, err := c.DescribeWorkflowExecution(ctx, backupWorkflowID, "")
	require.NoError(t, err, "describe workflow %s", backupWorkflowID)

	return description.GetWorkflowExecutionInfo().GetExecution().GetRunId()
}

func ptrFloat(f float64) *float64 { return &f }

// terminateOnCleanup best-effort terminates the backup run when the test ends, so a
// panicked, timed-out, or otherwise-abandoned run does not linger on the shared
// Temporal server — and, because the workflow ID is a singleton, so the next test
// can submit its own run. Terminating an already-closed run is a harmless no-op.
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

// execOut runs a command, returning its combined output and a wrapped error that
// includes that output for diagnosis.
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

// execStdout runs a command and returns only its stdout, so callers can parse it
// without stderr noise (nix/docker emit warnings to stderr that would otherwise
// corrupt a parsed path or container id). On error the wrapped message carries
// stderr for diagnosis.
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
