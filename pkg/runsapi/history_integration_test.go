//go:build integration

package runsapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/runsapi"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// TestHistoryEndpointsAgainstRealDryRun drives a complete, real dry-run
// (issue #273): the actual Backup workflow through real control + data
// workers against mhvtl and the ephemeral ZFS pool, submitted through POST
// /api/runs with dryRun=true (the same mhvtl redirection a browser
// submission gets), then exercises every history-derived endpoint against
// the finished run's real Temporal event history:
//
//   - GET /api/runs/{runID}/phases — all 11 phases completed in order, with
//     real per-phase facts (archive counts, tapes written) recovered from the
//     history's real activity payloads.
//   - GET /api/runs/{runID}/config — the exact submitted config round-trips
//     (with the age identity redacted).
//   - GET /api/runs/{runID}/tapes — the written tape's real barcode, indices,
//     slot, and write-health.
//   - GET /api/tapes — the aggregate listing includes the run's tape,
//     attributed back to the run, and returns 200 even though the shared dev
//     Temporal's visibility also holds this package's *stub* workflow
//     executions from sibling integration tests (foreign histories degrade
//     per-run, never failing the listing).
//   - unknown/malformed run IDs — 404/400, distinct from success.
//
// The aged-out-history 410 path cannot be exercised here (it requires
// Temporal's retention window to actually elapse); its classification is
// covered by unit tests (TestHistoryEndpointErrorClassification).
//
// Driven by `make test-integration`; skips when Temporal, mhvtl, LTFS, ZFS,
// or the recovery tooling is absent.
func TestHistoryEndpointsAgainstRealDryRun(t *testing.T) {
	requireTemporalAddress(t)
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)
	testutil.SkipIfPoolUnavailable(t)
	requireBinaries(t, "age", "age-keygen", "par2", "zstd", "mt", "sg_raw")

	snapshot := testutil.TestSnapshot(t)
	if snapshot == "" {
		t.Skipf("%s not set; run via `make test-integration`", testutil.EnvTestSnapshot)
	}

	// A dry-run submission requires the mhvtl env vars on the API server's
	// environment (runsubmit.ApplyDryRun); make test-integration sets them.
	for _, name := range []string{"MHVTL_CHANGER_DEV", "MHVTL_DRIVE0_DEV", "MHVTL_DRIVE1_DEV"} {
		if os.Getenv(name) == "" {
			t.Skipf("%s not set; run via `make test-integration`", name)
		}
	}

	source := testutil.PoolDataset(t) + "@" + snapshot

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inventory, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inventory.Drives), 1, "at least one drive required")
	require.False(t, inventory.Drives[0].Loaded, "drive 0 must start empty")

	// Storage slot index 3: slots 0-2 are claimed by workflows/backup's
	// session, tape-path, and e2e integration tests sharing this mhvtl
	// library (see e2e_integration_test.go's slot comment); -p 1 serializes
	// the package binaries but leftover state must still not collide.
	require.GreaterOrEqual(t, len(inventory.Slots), 4, "at least four storage slots required")
	slot := inventory.Slots[3]
	require.True(t, slot.Full, "slot 3 must hold a tape")
	require.NotEmpty(t, slot.Barcode, "slot 3 tape must have a barcode")

	// Resolve drive 0's st/sg nodes by SCSI target, not the MHVTL_DRIVE*_DEV
	// env defaults: the kernel assigns st/sg minors in probe order, which does
	// not reliably match mhvtl's drive numbering (mhvtl DTE0 is SCSI target 1
	// but its st node can come up as nst0 OR nst1 across module loads). The
	// pre-blanking below drives the physical DTE0, so it must use the node
	// that actually backs it — testutil resolves by target when the env var is
	// unset. The MHVTL_DRIVE* vars are then re-pointed at the resolved nodes
	// so the dry-run submission's config matches physical reality too (the
	// workflow's Load pairs drives to changer elements by unit serial, issue
	// #137, so it tolerates either order — the pre-blanking cannot).
	t.Setenv(testutil.EnvDrive0Dev, "")
	t.Setenv(testutil.EnvDrive1Dev, "")
	t.Setenv(testutil.EnvDrive0SgDev, "")

	stDevice := testutil.Drive0Dev(t)
	sgDevice := testutil.Drive0SgDev(t)

	t.Setenv(testutil.EnvDrive0Dev, stDevice)
	t.Setenv(testutil.EnvDrive1Dev, testutil.Drive1Dev(t))

	driveAddr := inventory.Drives[0].Address
	slotAddr := slot.Address
	barcode := slot.Barcode

	// Register cleanup BEFORE touching the drive (mirrors the e2e test):
	// whatever happens, leave drive 0 empty and the tape back in its slot.
	t.Cleanup(func() { returnTapeToStorageSlot(changer, slotAddr, driveAddr, barcode) })

	// The run must find a blank tape: pre-load, verify readiness, SCSI-erase,
	// unload — leaving a genuinely blank tape in slot 3.
	require.NoError(t, changer.Load(t.Context(), slotAddr, driveAddr), "pre-load for blanking")
	testutil.SkipIfDriveNotReady(t, stDevice)
	eraseLoadedTape(t.Context(), stDevice, sgDevice)
	require.NoError(t, changer.Unload(t.Context(), slotAddr, driveAddr), "unload after blanking")

	isolateTemporalConfig(t)

	temporalClient, shutdown, err := temporalclient.New(t.Context(), nil)
	require.NoError(t, err)

	defer shutdown()

	controlWorker := worker.New(temporalClient, backup.TaskQueue, worker.Options{})
	backup.RegisterControl(controlWorker, backup.ControlConfig{})
	require.NoError(t, controlWorker.Start(), "start control worker")
	t.Cleanup(controlWorker.Stop)

	dataWorker := worker.New(temporalClient, backup.DataTaskQueue, worker.Options{EnableSessionWorker: true})
	backup.RegisterData(dataWorker, backup.DataConfig{
		StagingDir:          t.TempDir(),
		RecoveryBinariesDir: testutil.RecoveryBinariesDir(t),
		RecoverySourcesDir:  testutil.RecoverySourcesDir(t),
	})
	require.NoError(t, dataWorker.Start(), "start data worker")
	t.Cleanup(dataWorker.Stop)

	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(webhook.Close)

	identity, recipient := generateAgeKeypair(t)

	cfg := config.Config{
		Sources: []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:  1,
		Library: config.Library{
			// The changer/drives here are placeholders the dry-run override
			// replaces with the mhvtl env-var nodes (runsubmit.ApplyDryRun);
			// the blank slot is a logical position and is used as-is.
			Changer:           testutil.ChangerDev(t),
			Drives:            []string{stDevice},
			BlankSlots:        []int{slotAddr},
			TapeCapacityBytes: 2_500_000_000_000,
		},
		Redundancy: config.Redundancy{TargetPercentage: floatPointer(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: webhook.URL},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	handler := runsapi.New(temporalClient)
	server := httptest.NewServer(handler)

	defer server.Close()

	configJSON, err := json.Marshal(cfg)
	require.NoError(t, err)

	submitBody, err := json.Marshal(map[string]interface{}{"config": json.RawMessage(configJSON), "dryRun": true})
	require.NoError(t, err)

	// Submit through the API's own dry-run path; tolerate a still-closing
	// leftover singleton run from a sibling test by retrying.
	var runID string

	require.Eventually(t, func() bool {
		status, body := postRun(t, server.URL, submitBody)
		if status != http.StatusCreated {
			return false
		}

		runID = body.RunID

		return runID != ""
	}, 60*time.Second, time.Second, "dry-run submission never started")

	// Wait for the real run to complete. The full pipeline against mhvtl
	// (tar → age → PAR2 → verify → mkltfs → LTFS write → eject → report →
	// deliver) takes a few minutes; bound it below `go test`'s own timeout.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 8*time.Minute)
	defer cancel()

	require.NoError(t, temporalClient.GetWorkflow(runCtx, backup.WorkflowID, runID).Get(runCtx, nil),
		"the dry-run must complete successfully")

	// --- GET /api/runs/{runID}/phases ---
	phasesStatus, phasesBody := getJSON[runsapi.RunPhasesResponse](t, server.URL+"/api/runs/"+runID+"/phases")
	require.Equal(t, http.StatusOK, phasesStatus)
	require.Len(t, phasesBody.Phases, 11, "all 11 pipeline phases must be reported")

	wantOrder := []string{
		backup.PhaseResolve, backup.PhasePrepare, backup.PhasePack, backup.PhaseGeneratePAR2,
		backup.PhaseVerify, backup.PhaseLoad, backup.PhaseWrite, backup.PhaseEject,
		backup.PhaseReport, backup.PhaseBurn, backup.PhaseDeliver,
	}

	phaseByName := make(map[string]runsapi.PhaseInfo, len(phasesBody.Phases))

	for i, phase := range phasesBody.Phases {
		assert.Equal(t, wantOrder[i], phase.Name, "phase order")
		assert.Equal(t, runsapi.PhaseCompleted, phase.Status, "phase %s must be completed", phase.Name)

		phaseByName[phase.Name] = phase
	}

	// Every phase that did real work has a real time window; Burn ran as a
	// no-op (dry-run disables optical burning).
	for _, name := range wantOrder {
		if name == backup.PhaseBurn {
			continue
		}

		assert.NotNil(t, phaseByName[name].StartTime, "phase %s start time", name)
		assert.NotNil(t, phaseByName[name].EndTime, "phase %s end time", name)
	}

	// Spot-check real per-phase facts recovered from the history (AC2).
	assertFact(t, phaseByName[backup.PhaseResolve].Facts, "archives", "1")
	assertFact(t, phaseByName[backup.PhasePack].Facts, "logicalTapes", "1")
	assertFact(t, phaseByName[backup.PhasePack].Facts, "copies", "1")
	assertFact(t, phaseByName[backup.PhaseGeneratePAR2].Facts, "recoverySets", "1")
	assertFact(t, phaseByName[backup.PhaseLoad].Facts, "tapesLoaded", "1")
	assertFact(t, phaseByName[backup.PhaseWrite].Facts, "tapesWritten", "1")
	assertFact(t, phaseByName[backup.PhaseBurn].Facts, "opticalBurn", "disabled")
	assertFact(t, phaseByName[backup.PhaseDeliver].Facts, "delivered", "yes")

	// --- GET /api/runs/{runID}/config (AC4) ---
	configStatus, configBody := getJSON[runsapi.RunConfigResponse](t, server.URL+"/api/runs/"+runID+"/config")
	require.Equal(t, http.StatusOK, configStatus)
	require.Len(t, configBody.Config.Sources, 1)
	assert.Equal(t, source, configBody.Config.Sources[0].ZFSPath.Name, "the submitted source must round-trip")
	assert.Equal(t, 1, configBody.Config.Copies)
	assert.Equal(t, []int{slotAddr}, configBody.Config.Library.BlankSlots)
	assert.Equal(t, []string{recipient}, configBody.Config.Encryption.Recipients)
	assert.NotEqual(t, identity, configBody.Config.Encryption.Identity,
		"the age private identity must never be returned")
	// The config endpoint returns the config as *submitted to Temporal* —
	// which for a dry-run is the post-ApplyDryRun config, mhvtl devices and
	// no optical burn (the exact run configuration the workflow executed).
	assert.Equal(t, os.Getenv("MHVTL_CHANGER_DEV"), configBody.Config.Library.Changer,
		"a dry-run's stored config carries the mhvtl override it actually ran with")

	// --- GET /api/runs/{runID}/tapes (AC5) ---
	tapesStatus, tapesBody := getJSON[runsapi.RunTapesResponse](t, server.URL+"/api/runs/"+runID+"/tapes")
	require.Equal(t, http.StatusOK, tapesStatus)
	require.Len(t, tapesBody.Tapes, 1, "exactly one physical tape was written")

	writtenTape := tapesBody.Tapes[0]
	assert.Equal(t, string(barcode), writtenTape.Barcode, "the real tape's barcode")
	assert.Equal(t, 0, writtenTape.TapeIndex)
	assert.Equal(t, 0, writtenTape.CopyIndex)
	assert.Equal(t, slotAddr, writtenTape.Slot)
	assert.Equal(t, "written", writtenTape.Result)
	require.NotNil(t, writtenTape.WriteHealth, "write-health must be recovered from the history")
	assert.Greater(t, writtenTape.WriteHealth.ThroughputMBps, 0.0)

	// --- GET /api/tapes (AC6) ---
	aggregateStatus, aggregateBody := getJSON[runsapi.AggregateTapesResponse](t, server.URL+"/api/tapes")
	require.Equal(t, http.StatusOK, aggregateStatus,
		"the aggregate listing must succeed even with sibling tests' stub runs in visibility")

	var found bool

	for _, aggregate := range aggregateBody.Tapes {
		if aggregate.RunID == runID && aggregate.Barcode == string(barcode) {
			found = true

			assert.Equal(t, "written", aggregate.Result)
			assert.False(t, aggregate.RunStartTime.IsZero(), "the tape must be attributable to its run's start time")
		}
	}

	assert.True(t, found, "the aggregate tapes listing must include this run's tape, attributed to the run")

	// --- unknown vs malformed run IDs (AC7) ---
	notFoundStatus, _ := getJSON[map[string]interface{}](t, server.URL+"/api/runs/00000000-0000-0000-0000-000000000000/phases")
	assert.Equal(t, http.StatusNotFound, notFoundStatus, "a never-existed run ID is 404")

	badStatus, _ := getJSON[map[string]interface{}](t, server.URL+"/api/runs/not-a-uuid/phases")
	assert.Equal(t, http.StatusBadRequest, badStatus, "a malformed run ID is 400")
}

// getJSON GETs url and decodes the response body into T, returning the
// status code and decoded body.
func getJSON[T any](t *testing.T, url string) (int, T) {
	t.Helper()

	response, err := http.Get(url)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	var body T

	_ = json.NewDecoder(response.Body).Decode(&body)

	return response.StatusCode, body
}

// assertFact asserts facts contains key with the given value.
func assertFact(t *testing.T, facts []runsapi.PhaseFact, key, want string) {
	t.Helper()

	for _, fact := range facts {
		if fact.Key == key {
			assert.Equal(t, want, fact.Value, "fact %s", key)

			return
		}
	}

	t.Errorf("fact %q not found among %+v", key, facts)
}

// requireBinaries skips the test when any named tool is not on PATH.
func requireBinaries(t *testing.T, names ...string) {
	t.Helper()

	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not on PATH (run within `nix develop`)", name)
		}
	}
}

// generateAgeKeypair generates a fresh post-quantum age keypair for the run's
// encryption config (the Report phase verifies the escrow identity matches a
// recipient, so a hardcoded fake would fail the run).
func generateAgeKeypair(t *testing.T) (identity, recipient string) {
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

// eraseLoadedTape issues a short SCSI ERASE to the loaded tape, resetting
// mhvtl's in-memory state to blank (the same best-effort helper the
// workflows/backup integration tests use).
func eraseLoadedTape(ctx context.Context, stDevice, sgDevice string) {
	rewindCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = exec.CommandContext(rewindCtx, "mt", "-f", stDevice, "rewind").Run()

	cancel()

	_ = exec.CommandContext(ctx, "sg_raw", sgDevice,
		"0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
}

// returnTapeToStorageSlot restores the library after the run: drive 0 empty
// and the tape back in its storage slot (from the drive if the run failed
// mid-write, or from the I/O station where Eject parked it). Best-effort.
func returnTapeToStorageSlot(changer *tape.Changer, slotAddr, driveAddr int, barcode tape.Barcode) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	inventory, err := changer.Inventory(cleanupCtx)
	if err != nil {
		return
	}

	if len(inventory.Drives) > 0 && inventory.Drives[0].Loaded {
		_ = changer.Unload(cleanupCtx, slotAddr, driveAddr)

		return
	}

	for _, ioSlot := range inventory.IOSlots {
		if ioSlot.Full && ioSlot.Barcode == barcode {
			_ = changer.Transfer(cleanupCtx, ioSlot.Address, slotAddr)

			return
		}
	}
}

// floatPointer returns a pointer to f, for optional config fields.
func floatPointer(f float64) *float64 { return &f }
