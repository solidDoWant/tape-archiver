//go:build e2e

// TestWebUIEndToEnd (this file) is the web UI's own end-to-end test (issue
// #260, sub-issue 9 of the web UI epic #239): a real headless browser
// (Playwright, web/e2e/) driven against cmd/web deployed the way it ships in
// production — its own Helm chart (deploy/charts/tape-archiver-web) + image
// — into the SAME kind cluster + mhvtl + dev Temporal topology the rest of
// this package's tests already stand up (harness_test.go), rather than a
// second, duplicated topology. It additionally deploys the web chart, starts
// an in-process fake OIDC provider (no real IdP exists in this sandbox —
// internal/testutil.NewFakeOIDCProvider, already reused by cmd/web's own
// integration tests), and port-forwards the web Service to the host, then
// shells out to `npx playwright test` (web/e2e/, driven by
// web/playwright.config.ts) to do the actual browser-driving.
//
// This is a per-test prerequisite check (webE2EPrereqReason), not part of
// checkPrerequisites/setupHarness: a missing Node/Playwright/Chromium
// toolchain skips only this test, not the whole shared harness (and thus
// not the rest of the backup-workflow e2e suite in this same binary).
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

// webRepo is the web image's published name, mirroring controlRepo/dataRepo
// above (harness_test.go) — kept local to this file since only this test
// needs it.
const webRepo = "ghcr.io/soliddowant/tape-archiver/web"

// webChartDir/webHelmRelease/webK8sNamespace/webSecretName name the chart
// path, the Helm release, and the Secret this test creates for the web UI's
// OIDC client secret + session key — separate from the control worker's own
// helmRelease/namespace constants above so `helm uninstall`/`kubectl delete`
// here can never touch the control worker's release.
const (
	webChartDir     = "deploy/charts/tape-archiver-web"
	webHelmRelease  = "ta-web-e2e"
	webK8sNamespace = namespace // "default" — same namespace the control worker deploys into
	webSecretName   = "ta-web-e2e-secrets"

	// webOIDCClientID/webOIDCClientSecret are the confidential-client
	// credentials this test's fake OIDC provider and the deployed web chart
	// both use — fixed, non-sensitive values (this whole topology is
	// throwaway and torn down at the end of the test).
	webOIDCClientID     = "tape-archiver-web-e2e"
	webOIDCClientSecret = "web-e2e-fake-client-secret"

	// webBlankSlot is the mhvtl storage slot this test blanks and uses. It
	// must not collide with any slot index the other e2e tests in this
	// package use — see tape_test.go's prepareBlankTapeAt callers (2, 3, 4,
	// 5, 6, 8, 9, 12+, 20+, 24, 26) — comfortably clear of all of them.
	webBlankSlot = 30

	// webFormBlankSlot is a second, separate blank slot for
	// config-form-and-tapes.spec.ts's own dry-run submission (issue #281) —
	// distinct from webBlankSlot above, which submit-monitor-history.spec.ts
	// already writes to; the two specs run sequentially against the same
	// singleton `backup` workflow (SPEC §4.2, playwright.config.ts's
	// workers: 1), so their tapes must not collide.
	webFormBlankSlot = 31
)

// webE2EPrereqReason returns a skip reason when a prerequisite specific to
// the Playwright suite (beyond the shared harness's own — checkPrerequisites
// already covers docker/kind/helm/nix/mhvtl/Temporal) is unavailable, or ""
// when the suite can run. Checked lazily, only by TestWebUIEndToEnd, so a
// missing Node/Playwright/Chromium toolchain never fails — or slows down —
// the rest of the e2e suite in this same binary (AC3).
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

// TestWebUIEndToEnd is the whole-stack browser test: OIDC login -> submit a
// dry-run config through the real form -> watch its phase update live via
// SSE, with no manual reload -> see it (while Running, and again once
// Completed) in the run-history view. See this file's package doc comment
// for the topology and internal/testutil/oidc.go's NewFakeOIDCProviderOn for
// how the fake IdP is made reachable from both the kind cluster and this
// host process's browser.
func TestWebUIEndToEnd(t *testing.T) {
	h := requireHarness(t)

	if reason := webE2EPrereqReason(h.repoRoot); reason != "" {
		t.Skipf("web e2e prerequisites unavailable: %s", reason)
	}

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	fixture := prepareBlankTapeAt(t, webBlankSlot)
	// config-form-and-tapes.spec.ts's Form-mode submission needs its own
	// separate blank slot (typed directly into the page, not read from a
	// config file) — prepareBlankTapeAt's own assertions (drive empty, slot
	// holds a tape) are exactly the precondition check that submission
	// needs too; only the slot number itself (not the rest of the fixture)
	// is passed through, since the dry-run toggle overwrites
	// Library.Changer/Drives server-side regardless of what the form typed.
	prepareBlankTapeAt(t, webFormBlankSlot)
	identity, recipient := generateTestKeypair(t)

	deliveryToken := fmt.Sprintf("web-e2e-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:     1,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(deliveryToken)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	// Every other submitting e2e test in this package pairs its submission
	// with terminateOnCleanup — the backup workflow ID is a singleton, so an
	// abandoned run (a Playwright assertion failing/timing out before the run
	// reaches Completed) would otherwise keep running while this test's own
	// tape-unload cleanup and the shared harness's final data-worker teardown
	// fire, risking a race against an in-flight Load/Write on the same tape
	// (CLAUDE.md's "never write to a non-blank tape" / handle physical
	// operations with care). Registered here, before the run is ever
	// submitted through the browser, so it applies regardless of how far the
	// test gets.
	terminateOnCleanup(t, dialTemporal(t))

	// The web form's dry-run toggle overwrites Library.Changer/Drives with
	// whatever MHVTL_*_DEV values the deployed web chart carries (below) —
	// pkg/runsubmit.ApplyDryRun — so this config's own device paths are only
	// there to satisfy cfg.Validate() above; what actually reaches the
	// workflow comes from the chart's config.web.dryRun.* values, set to the
	// same real mhvtl devices the shared data worker container already has
	// (harness_test.go's startDataWorker).
	configData, err := json.Marshal(cfg)
	require.NoError(t, err, "marshal run config")

	configPath := filepath.Join(t.TempDir(), "run-config.json")
	require.NoError(t, os.WriteFile(configPath, configData, 0o600))

	// --- kind: load the web image (already built by setupHarness's shared
	// buildImages step, since `make build-images` builds it alongside the
	// worker images) ---
	_, err = execOut(h.repoRoot, nil, "kind", "load", "docker-image", webRepo+":"+imageVersion, "--name", clusterName)
	require.NoError(t, err, "kind load web image")

	// --- fake OIDC provider, reachable from both kind pods and this host's
	// browser via the kind bridge gateway IP. Binds 0.0.0.0 and advertises the
	// gateway IP separately, the same as startWebhook above — not a listener
	// bound directly to h.gatewayIP: that address is only guaranteed to be
	// reachable (any docker bridge peer can dial it), not guaranteed to be
	// locally bindable on every Docker network topology (e.g. rootless
	// Docker/dockerd-rootless, or Docker running inside its own network
	// namespace, can make the bridge gateway a proxied address this process's
	// own network namespace cannot own as a local interface). Binding all
	// interfaces avoids that failure mode entirely.
	idpListener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err, "listen for fake OIDC provider")

	idp := testutil.NewFakeOIDCProviderOn(t, webOIDCClientID, webOIDCClientSecret, idpListener)
	idp.Server.URL = fmt.Sprintf("http://%s:%d", h.gatewayIP, idpListener.Addr().(*net.TCPAddr).Port)

	// --- reserve the local port the browser will use, and bake it into the
	// OIDC redirect URL before the chart (which bakes OIDC_REDIRECT_URL into
	// the pod's env) is installed ---
	localPort := freeLocalPort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", localPort)
	redirectURL := baseURL + "/auth/callback"

	// --- Secret: OIDC client secret + session key ---
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

	// --- deploy the web chart ---
	_, err = execOut(h.repoRoot, nil, "helm", "dependency", "build", webChartDir)
	require.NoError(t, err, "helm dependency build (web chart)")

	imagePath := "resources.controllers.main.containers.main.image"

	// Register the uninstall before installing (matching installScaledJobWorker's
	// documented reasoning above) so a failed/partial install — e.g. a --wait
	// timeout, a realistic CI scenario — still cleans up whatever the release
	// did manage to create, instead of leaving a stray "ta-web-e2e" release
	// (and its Secret) in the shared kind cluster for the rest of the process.
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
		// Deploy-owned library devices the guided Form mode sources read-only
		// (issue #304): the Form-mode e2e spec builds a config that must pass the
		// Review step's client-side validation (changer non-empty, >=1 drive), so
		// the deployed cmd/web has to serve real values via GET /api/config/ui.
		// The dry-run submission still redirects to the mhvtl nodes server-side
		// (pkg/runsubmit.ApplyDryRun), so these need only be non-empty/valid.
		"--set", "config.web.library.changer="+testutil.ChangerDev(t),
		"--set", "config.web.library.drives={"+testutil.Drive0Dev(t)+","+testutil.Drive1Dev(t)+"}",
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

	// --- run the real Playwright suite against the deployed, port-forwarded
	// web UI ---
	playwrightCtx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(playwrightCtx, "npx", "playwright", "test")
	cmd.Dir = filepath.Join(h.repoRoot, "web")

	cmd.Env = append(os.Environ(),
		"WEB_UI_BASE_URL="+baseURL,
		"RUN_CONFIG_PATH="+configPath,
		"FORM_ZFS_SOURCE="+source,
		"FORM_BLANK_SLOT="+strconv.Itoa(webFormBlankSlot),
	)

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "playwright e2e suite failed:\n%s", out)

	t.Logf("playwright output:\n%s", out)
}

// freeLocalPort reserves an available TCP port on 127.0.0.1 by binding and
// immediately closing a listener, returning just the port number. There is
// an inherent (accepted, matching an existing pattern elsewhere in this
// repo — cmd/web/main_integration_test.go's freeAddr) TOCTOU race between
// this and whatever later binds the same port; acceptable for test infra.
func freeLocalPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	return port
}

// startKubectlPortForward starts `kubectl port-forward` for target (e.g.
// "svc/my-service") in the background, waits for it to report ready
// ("Forwarding from ..."), and registers a t.Cleanup that kills it. Output
// after readiness is drained via log.Printf (not t.Log — this runs from a
// goroutine that outlives the synchronous portion of the test, and t.Log
// from a goroutine after the test could otherwise return is unsafe).
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
