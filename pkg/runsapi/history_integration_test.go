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

// TestHistoryEndpointsAgainstRealDryRun runs a real dry-run against mhvtl and
// checks every history-derived endpoint (issue #273). The 410 aged-out path
// needs retention to elapse, so it is unit-tested only.
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

	// The drive variables are resolved by SCSI address below.
	if os.Getenv("MHVTL_CHANGER_DEV") == "" {
		t.Skip("MHVTL_CHANGER_DEV not set; run via `make test-integration`")
	}

	source := testutil.PoolDataset(t) + "@" + snapshot

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inventory, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inventory.Drives), 1, "at least one drive required")
	require.False(t, inventory.Drives[0].Loaded, "drive 0 must start empty")

	// Slots 0-2 belong to workflows/backup's integration tests on the same
	// library.
	require.GreaterOrEqual(t, len(inventory.Slots), 4, "at least four storage slots required")
	slot := inventory.Slots[3]
	require.True(t, slot.Full, "slot 3 must hold a tape")
	require.NotEmpty(t, slot.Barcode, "slot 3 tape must have a barcode")

	// Resolve drive 0's nodes by SCSI target: st/sg minors follow probe order,
	// so DTE0 may be nst0 or nst1, and pre-blanking must hit the physical DTE0.
	// The env vars are then re-pointed so the dry-run config matches.
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

	// Register cleanup before touching the drive.
	t.Cleanup(func() { returnTapeToStorageSlot(changer, slotAddr, driveAddr, barcode) })

	// Blank the tape so the run finds a blank one.
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
			// Devices are replaced by the dry-run override; the slot is kept.
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

	// Free the singleton, then submit once: POST /api/runs uses a FAIL conflict
	// policy, so retrying cannot make progress (issue #338).
	freeBackupSingleton(t, t.Context(), temporalClient)

	status, submitted, raw := postRun(t, server.URL, submitBody)
	require.Equal(t, http.StatusCreated, status, "dry-run submission failed: %s", raw)

	runID := submitted.RunID
	require.NotEmpty(t, runID, "dry-run submission returned no run ID: %s", raw)

	// Bound below `go test`'s own timeout.
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

	// Burn is a no-op in dry-run.
	for _, name := range wantOrder {
		if name == backup.PhaseBurn {
			continue
		}

		assert.NotNil(t, phaseByName[name].StartTime, "phase %s start time", name)
		assert.NotNil(t, phaseByName[name].EndTime, "phase %s end time", name)
	}

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
	// A dry-run's stored config is the post-ApplyDryRun one.
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
	assert.True(t, writtenTape.WriteHealth.Measured)
	// The 8 MB fixture pool is far too small to judge streaming (#409).
	assert.True(t, writtenTape.WriteHealth.ThroughputUnmeasurable,
		"a write this small must report throughput as not measurable, not a near-zero rate")

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

func getJSON[T any](t *testing.T, url string) (int, T) {
	t.Helper()

	response, err := http.Get(url)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	var body T

	_ = json.NewDecoder(response.Body).Decode(&body)

	return response.StatusCode, body
}

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

func requireBinaries(t *testing.T, names ...string) {
	t.Helper()

	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not on PATH (run within `nix develop`)", name)
		}
	}
}

// generateAgeKeypair makes a real keypair: Report verifies the identity matches
// a recipient.
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

// eraseLoadedTape issues a best-effort short SCSI ERASE to blank the tape.
func eraseLoadedTape(ctx context.Context, stDevice, sgDevice string) {
	rewindCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = exec.CommandContext(rewindCtx, "mt", "-f", stDevice, "rewind").Run()

	cancel()

	_ = exec.CommandContext(ctx, "sg_raw", sgDevice,
		"0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
}

// returnTapeToStorageSlot best-effort returns the tape from the drive or I/O
// station to its slot.
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

func floatPointer(f float64) *float64 { return &f }
