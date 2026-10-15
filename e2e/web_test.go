//go:build e2e

// TestWebUIEndToEnd (issue #260) drives Playwright (web/e2e/) against cmd/web
// deployed via its Helm chart into the shared harness topology, with a fake
// OIDC provider and a port-forward. Its toolchain prerequisites are checked
// per test, so a missing Node/Playwright skips only this test.
package e2e

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
)

const webRepo = "ghcr.io/soliddowant/tape-archiver/web"

// These are distinct from the control worker's so cleanup here can never
// touch its release.
const (
	webChartDir     = "deploy/charts/tape-archiver-web"
	webHelmRelease  = "ta-web-e2e"
	webK8sNamespace = namespace // "default" — same namespace the control worker deploys into
	webSecretName   = "ta-web-e2e-secrets"

	// Fixed, non-sensitive credentials for the throwaway fake OIDC provider.
	webOIDCClientID     = "tape-archiver-web-e2e"
	webOIDCClientSecret = "web-e2e-fake-client-secret"

	// webBlankSlot is a slot index (element address +1) that must not collide
	// with other tests' prepareBlankTapeAt slots (2-6, 8, 9, 12+, 20+, 24, 26).
	webBlankSlot = 30

	// webFormBlankSlot is config-form-and-tapes.spec.ts's own blank slot
	// (issue #281), so its tape never collides with webBlankSlot's.
	webFormBlankSlot = 31

	// webLibrarySlotCount mirrors mhvtl's 47 storage slots so the Form-mode slot
	// grid (issue #305/#310) can select webFormBlankSlot.
	webLibrarySlotCount = 47
)

// webE2EPrereqReason returns a skip reason for missing Playwright-specific
// prerequisites, or "". Checked lazily so it never affects other tests (AC3).
func webE2EPrereqReason(repoRoot string) string {
	for _, tool := range []string{"npx", "npm", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Sprintf("%q not on PATH (run within `nix develop`)", tool)
		}
	}

	if _, err := os.Stat(filepath.Join(repoRoot, "web", "node_modules", "@playwright", "test")); err != nil {
		return "web/node_modules/@playwright/test is not installed; run `make build` or `make test` first (or `npm ci` in web/)"
	}

	browsersPath := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if browsersPath == "" {
		return "PLAYWRIGHT_BROWSERS_PATH is not set (run within `nix develop`; Playwright's own browser download does not run in this sandbox — see flake.nix)"
	}

	if _, err := os.Stat(browsersPath); err != nil {
		return fmt.Sprintf("PLAYWRIGHT_BROWSERS_PATH=%s does not exist", browsersPath)
	}

	return ""
}

// TestWebUIEndToEnd: OIDC login, submit a dry-run through the form, watch
// phases update live via SSE, and see the run in history.
func TestWebUIEndToEnd(t *testing.T) {
	h := requireHarness(t)

	if reason := webE2EPrereqReason(h.repoRoot); reason != "" {
		t.Skipf("web e2e prerequisites unavailable: %s", reason)
	}

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	fixture := prepareBlankTapeAt(t, webBlankSlot)
	// Only the element address is passed on; dry-run overwrites the devices
	// server-side.
	formFixture := prepareBlankTapeAt(t, webFormBlankSlot)
	identity, recipient := generateTestKeypair(t)

	deliveryToken := fmt.Sprintf("web-e2e-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:     1,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(deliveryToken)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	// Registered before submission: an abandoned run would otherwise race the
	// tape-unload cleanup with an in-flight Load/Write on the same tape.
	terminateOnCleanup(t, dialTemporal(t))

	// Dry-run (pkg/runsubmit.ApplyDryRun) replaces these devices with the
	// chart's config.web.dryRun.* values; they only need to validate.
	configData, err := json.Marshal(cfg)
	require.NoError(t, err, "marshal run config")

	configPath := filepath.Join(t.TempDir(), "run-config.json")
	require.NoError(t, os.WriteFile(configPath, configData, 0o600))

	// The web image is built by the shared buildImages step.
	_, err = execOut(h.repoRoot, nil, "kind", "load", "docker-image", webRepo+":"+imageVersion, "--name", clusterName)
	require.NoError(t, err, "kind load web image")

	// The fake OIDC provider binds 0.0.0.0 and advertises the gateway IP: the
	// gateway is reachable but not always locally bindable (e.g. rootless Docker).
	idpListener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err, "listen for fake OIDC provider")

	idp := testutil.NewFakeOIDCProviderOn(t, webOIDCClientID, webOIDCClientSecret, idpListener)
	idp.Server.URL = fmt.Sprintf("http://%s:%d", h.gatewayIP, idpListener.Addr().(*net.TCPAddr).Port)

	// The browser's port must be known before install: the chart bakes it into
	// OIDC_REDIRECT_URL.
	localPort := freeLocalPort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", localPort)
	redirectURL := baseURL + "/auth/callback"

	sessionKey := make([]byte, 32)
	_, err = rand.Read(sessionKey)
	require.NoError(t, err)

	_, _ = execOut(h.repoRoot, h.kubeEnv(), "kubectl", "delete", "secret", webSecretName,
		"-n", webK8sNamespace, "--ignore-not-found")
	_, err = execOut(h.repoRoot, h.kubeEnv(), "kubectl", "create", "secret", "generic", webSecretName,
		"-n", webK8sNamespace,
		"--from-literal=clientSecret="+webOIDCClientSecret,
		"--from-literal=sessionKey="+base64.StdEncoding.EncodeToString(sessionKey),
	)
	require.NoError(t, err, "create web UI OIDC/session Secret")

	t.Cleanup(func() {
		_, _ = execOut(h.repoRoot, h.kubeEnv(), "kubectl", "delete", "secret", webSecretName,
			"-n", webK8sNamespace, "--ignore-not-found")
	})

	_, err = execOut(h.repoRoot, nil, "helm", "dependency", "build", webChartDir)
	require.NoError(t, err, "helm dependency build (web chart)")

	imagePath := "resources.controllers.main.containers.main.image"

	// Registered before install so a partial install is still cleaned up.
	t.Cleanup(func() {
		_, _ = execOut(h.repoRoot, h.kubeEnv(), "helm", "uninstall", webHelmRelease,
			"--namespace", webK8sNamespace, "--wait", "--timeout", "2m", "--ignore-not-found")
	})

	_, err = execOut(h.repoRoot, h.kubeEnv(), "helm", "install", webHelmRelease, webChartDir,
		"--namespace", webK8sNamespace, "--create-namespace", "--wait", "--timeout", "4m",
		"--set", "config.temporal.address="+temporalAlias+":"+temporalPort,
		"--set", "config.web.oidc.issuerUrl="+idp.Server.URL,
		"--set", "config.web.oidc.clientId="+webOIDCClientID,
		"--set", "config.web.oidc.redirectUrl="+redirectURL,
		"--set", "config.web.oidc.clientSecret.secretKeyRef.name="+webSecretName,
		"--set", "config.web.oidc.clientSecret.secretKeyRef.key=clientSecret",
		"--set", "config.web.sessionKey.secretKeyRef.name="+webSecretName,
		"--set", "config.web.sessionKey.secretKeyRef.key=sessionKey",
		"--set", "config.web.dryRun.mhvtlChangerDev="+testutil.ChangerDev(t),
		"--set", "config.web.dryRun.mhvtlDrive0Dev="+testutil.Drive0Dev(t),
		"--set", "config.web.dryRun.mhvtlDrive1Dev="+testutil.Drive1Dev(t),
		// Form mode needs non-empty devices to pass client-side validation
		// (issue #304); dry-run redirects them to mhvtl server-side.
		"--set", "config.web.library.changer="+testutil.ChangerDev(t),
		"--set", "config.web.library.drives={"+testutil.Drive0Dev(t)+","+testutil.Drive1Dev(t)+"}",
		// Without a slot count the Form-mode grid has no slot to click (issue #305/#310).
		"--set", "config.web.library.topology.slotCount="+strconv.Itoa(webLibrarySlotCount),
		"--set-string", imagePath+".repository="+webRepo,
		"--set-string", imagePath+".tag="+imageVersion,
		"--set", imagePath+".pullPolicy=Never",
	)
	require.NoError(t, err, "helm install web chart")

	svcName, err := execStdout(h.repoRoot, h.kubeEnv(), "kubectl", "get", "svc",
		"-n", webK8sNamespace, "-l", "app.kubernetes.io/instance="+webHelmRelease,
		"-o", "jsonpath={.items[0].metadata.name}")
	require.NoError(t, err, "discover web UI Service name")

	svcName = strings.TrimSpace(svcName)
	require.NotEmpty(t, svcName, "web UI Service must exist after a successful helm install --wait")

	startKubectlPortForward(t, h.kubeconfig, webK8sNamespace, "svc/"+svcName, localPort, 8080)

	playwrightCtx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(playwrightCtx, "npx", "playwright", "test")
	cmd.Dir = filepath.Join(h.repoRoot, "web")

	cmd.Env = append(os.Environ(),
		"WEB_UI_BASE_URL="+baseURL,
		"RUN_CONFIG_PATH="+configPath,
		"FORM_ZFS_SOURCE="+source,
		"FORM_BLANK_SLOT="+strconv.Itoa(formFixture.slotAddr),
	)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "playwright e2e suite failed:\n%s", out)

	t.Logf("playwright output:\n%s", out)
}

// freeLocalPort returns a free 127.0.0.1 port (with an accepted TOCTOU race).
func freeLocalPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	return port
}

// startKubectlPortForward runs `kubectl port-forward` until ready and kills it on
// cleanup. Later output goes to log.Printf, since t.Log after return is unsafe.
func startKubectlPortForward(t *testing.T, kubeconfig, k8sNamespace, target string, localPort, remotePort int) {
	t.Helper()

	cmd := exec.Command("kubectl", "--kubeconfig", kubeconfig, "-n", k8sNamespace,
		"port-forward", target, fmt.Sprintf("%d:%d", localPort, remotePort), "--address", "127.0.0.1")

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	cmd.Stderr = cmd.Stdout

	require.NoError(t, cmd.Start(), "start kubectl port-forward")

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}

		_ = cmd.Wait()
	})

	ready := make(chan struct{}, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()

			log.Printf("[e2e] kubectl port-forward: %s", line)

			if strings.Contains(line, "Forwarding from") {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}
	}()

	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("kubectl port-forward did not report ready within 30s")
	}
}
