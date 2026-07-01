//go:build integration

package backup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
)

// TestBackupEndToEnd runs the whole backup workflow through a real Temporal
// server and real control + data workers, against mhvtl and the ephemeral ZFS
// pool: it stages a real ZFS snapshot, packs it, generates PAR2, verifies,
// writes it to a virtual tape, ejects it, builds the report and recovery ISO, and
// delivers both to a local webhook. It asserts every one of the ten phases
// (SPEC §4.3) completes in order and the run succeeds.
//
// Covers issue #55 AC3 (all 10 phases execute in order, success) and AC4 (the
// integration test passes against mhvtl + dev Temporal and skips when either —
// or ZFS/LTFS — is absent). Driven by `make test-integration`.
func TestBackupEndToEnd(t *testing.T) {
	requireTemporalAddress(t)
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)
	testutil.SkipIfPoolUnavailable(t)
	requireBinaries(t, "age", "age-keygen", "par2", "zstd")

	snapshot := testutil.TestSnapshot(t)
	if snapshot == "" {
		t.Skipf("%s not set; run via `make test-integration`", testutil.EnvTestSnapshot)
	}

	source := testutil.PoolDataset(t) + "@" + snapshot

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least one drive required")
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")

	// Use storage slot index 2 so this test does not collide with the session
	// (slot 0) and tape-path (slot 1) integration tests sharing the mhvtl library.
	require.GreaterOrEqual(t, len(inv.Slots), 3, "at least three storage slots required")
	slot := inv.Slots[2]
	require.True(t, slot.Full, "slot 2 must hold a tape")
	require.NotEmpty(t, slot.Barcode, "slot 2 tape must have a barcode")

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)
	driveAddr := inv.Drives[0].Address
	slotAddr := slot.Address
	barcode := slot.Barcode

	// The run must load a blank tape. Load the chosen tape, confirm the drive is
	// ready (skip if mhvtl left it stuck "not ready"), erase it to blank, and
	// unload — leaving a genuinely blank tape in slot 2 for the run to load.
	require.NoError(t, changer.Load(t.Context(), slotAddr, driveAddr), "pre-load for readiness/blanking")
	testutil.SkipIfDriveNotReady(t, stDev)
	eraseLoadedTape(t.Context(), stDev, sgDev)
	require.NoError(t, changer.Unload(t.Context(), slotAddr, driveAddr), "unload after blanking")

	// Return the tape to slot 2 and blank it again after the run so consecutive
	// runs start from the same clean state (the run leaves it in an I/O slot,
	// formatted). Mirrors the tape-path test's cleanup.
	t.Cleanup(func() { returnAndBlank(changer, stDev, sgDev, slotAddr, driveAddr, barcode) })

	temporalClient := dialTemporal(t)

	stagingDir := t.TempDir()
	binariesDir := testutil.RecoveryBinariesDir(t)

	controlWorker := worker.New(temporalClient, TaskQueue, worker.Options{})
	RegisterControl(controlWorker, ControlConfig{})
	require.NoError(t, controlWorker.Start(), "start control worker")
	t.Cleanup(controlWorker.Stop)

	dataWorker := worker.New(temporalClient, DataTaskQueue, worker.Options{EnableSessionWorker: true})
	RegisterData(dataWorker, DataConfig{StagingDir: stagingDir, RecoveryBinariesDir: binariesDir})
	require.NoError(t, dataWorker.Start(), "start data worker")
	t.Cleanup(dataWorker.Stop)

	var uploads atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, formErr := r.FormFile("files[0]"); formErr == nil {
			uploads.Add(1)
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	identity, recipient := generateTestKeypair(t)

	cfg := config.Config{
		Sources: []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:  1,
		Library: config.Library{
			Changer:           testutil.ChangerDev(t),
			Drives:            []string{stDev},
			BlankSlots:        []int{slotAddr},
			TapeCapacityBytes: 2_500_000_000_000,
		},
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: server.URL},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
	defer cancel()

	options := client.StartWorkflowOptions{
		ID:        fmt.Sprintf("e2e-backup-%d", time.Now().UnixNano()),
		TaskQueue: TaskQueue,
	}

	run, err := temporalClient.ExecuteWorkflow(runCtx, options, WorkflowType, cfg)
	require.NoError(t, err, "start workflow")

	var result Result
	require.NoError(t, run.Get(runCtx, &result), "workflow must complete successfully")

	// AC3: all ten phases ran to completion, in order.
	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete in order")

	// AC1/AC2: the Deliver phase uploaded both artifacts (report + compressed ISO).
	assert.Equal(t, int32(2), uploads.Load(), "report and recovery ISO must both be delivered")
}

// requireTemporalAddress skips the test when no Temporal server is configured.
func requireTemporalAddress(t *testing.T) {
	t.Helper()

	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; run via `make test-integration`")
	}
}

// dialTemporal connects to the Temporal server named by TEMPORAL_ADDRESS,
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

// requireBinaries skips the test when any named tool is not on PATH.
func requireBinaries(t *testing.T, names ...string) {
	t.Helper()

	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not on PATH (run within `nix develop`)", name)
		}
	}
}

// eraseLoadedTape issues a short SCSI ERASE (CDB 0x19, LONG=0) to the tape
// currently loaded in the drive, resetting mhvtl's in-memory state to blank
// without a long physical erase. It rewinds first (bounded) so ERASE starts at
// BOT. Best-effort: failures are ignored, mirroring the tape-path test.
func eraseLoadedTape(ctx context.Context, stDev, sgDev string) {
	rewindCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = exec.CommandContext(rewindCtx, "mt", "-f", stDev, "rewind").Run()

	cancel()

	_ = exec.CommandContext(ctx, "sg_raw", sgDev,
		"0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
}

// returnAndBlank returns the run's tape to its storage slot and blanks it so
// consecutive test runs start from the same clean state. The run leaves the tape
// in an I/O slot (after Eject), formatted; this moves it back and erases it.
func returnAndBlank(changer *tape.Changer, stDev, sgDev string, slotAddr, driveAddr int, barcode tape.Barcode) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	inv, err := changer.Inventory(cleanupCtx)
	if err != nil {
		return
	}

	loaded := len(inv.Drives) > 0 && inv.Drives[0].Loaded && inv.Drives[0].Barcode == barcode

	if !loaded {
		for _, io := range inv.IOSlots {
			if io.Full && io.Barcode == barcode {
				_ = changer.Transfer(cleanupCtx, io.Address, slotAddr)

				break
			}
		}

		if err := changer.Load(cleanupCtx, slotAddr, driveAddr); err != nil {
			return
		}
	}

	eraseLoadedTape(cleanupCtx, stDev, sgDev)
	_ = changer.Unload(cleanupCtx, slotAddr, driveAddr)
}
