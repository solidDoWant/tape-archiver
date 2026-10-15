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

// TestBackupEndToEnd runs the whole workflow through real Temporal and workers
// against mhvtl and the ephemeral ZFS pool, asserting every phase completes in
// order (issue #55 AC3, AC4).
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

	// Slot 2 avoids the session (0) and tape-path (1) integration tests.
	require.GreaterOrEqual(t, len(inv.Slots), 3, "at least three storage slots required")
	slot := inv.Slots[2]
	require.True(t, slot.Full, "slot 2 must hold a tape")
	require.NotEmpty(t, slot.Barcode, "slot 2 tape must have a barcode")

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)
	driveAddr := inv.Drives[0].Address
	slotAddr := slot.Address
	barcode := slot.Barcode

	// Registered before touching the drive so even an early exit leaves drive 0
	// empty and the tape in its slot, as sibling tests expect.
	t.Cleanup(func() { returnTapeToSlot(changer, slotAddr, driveAddr, barcode) })

	// The run requires a blank tape: load, skip if mhvtl is stuck "not ready",
	// erase, and unload.
	require.NoError(t, changer.Load(t.Context(), slotAddr, driveAddr), "pre-load for readiness/blanking")
	testutil.SkipIfDriveNotReady(t, stDev)
	eraseLoadedTape(t.Context(), stDev, sgDev)
	require.NoError(t, changer.Unload(t.Context(), slotAddr, driveAddr), "unload after blanking")

	temporalClient := dialTemporal(t)

	stagingDir := t.TempDir()
	binariesDir := testutil.RecoveryBinariesDir(t)
	sourcesDir := testutil.RecoverySourcesDir(t)

	// Mirror production worker options (cmd/worker.workerOptions).
	controlWorker := worker.New(temporalClient, TaskQueue, worker.Options{})
	RegisterControl(controlWorker, ControlConfig{})
	require.NoError(t, controlWorker.Start(), "start control worker")
	t.Cleanup(controlWorker.Stop)

	dataWorker := worker.New(temporalClient, DataTaskQueue, worker.Options{EnableSessionWorker: true})
	RegisterData(dataWorker, DataConfig{StagingDir: stagingDir, RecoveryBinariesDir: binariesDir, RecoverySourcesDir: sourcesDir})
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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: server.URL},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	// Below go test's 10m default, so a stall fails with a clear deadline error.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 6*time.Minute)
	defer cancel()

	options := client.StartWorkflowOptions{
		ID:        fmt.Sprintf("e2e-backup-%d", time.Now().UnixNano()),
		TaskQueue: TaskQueue,
	}

	run, err := temporalClient.ExecuteWorkflow(runCtx, options, WorkflowType, cfg)
	require.NoError(t, err, "start workflow")

	var result Result
	require.NoError(t, run.Get(runCtx, &result), "workflow must complete successfully")

	assert.Equal(t, orderedPhases, result.CompletedPhases, "all pipeline phases must complete in order")

	// Only the report: no optical burning, so no ISO.
	assert.Equal(t, int32(1), uploads.Load(), "only the report must be delivered")
}

// requireTemporalAddress skips the test when no Temporal server is configured.
func requireTemporalAddress(t *testing.T) {
	t.Helper()

	if os.Getenv("TEMPORAL_ADDRESS") == "" {
		t.Skip("TEMPORAL_ADDRESS not set; run via `make test-integration`")
	}
}

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

// requireBinaries skips the test when any named tool is not on PATH.
func requireBinaries(t *testing.T, names ...string) {
	t.Helper()

	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not on PATH (run within `nix develop`)", name)
		}
	}
}

// eraseLoadedTape issues a short SCSI ERASE (CDB 0x19, LONG=0), resetting mhvtl's
// in-memory tape to blank without a long erase. Best-effort.
func eraseLoadedTape(ctx context.Context, stDev, sgDev string) {
	rewindCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = exec.CommandContext(rewindCtx, "mt", "-f", stDev, "rewind").Run()

	cancel()

	_ = exec.CommandContext(ctx, "sg_raw", sgDev,
		"0x19", "0x00", "0x00", "0x00", "0x00", "0x00").Run()
}

// returnTapeToSlot leaves drive 0 empty and the tape in its storage slot, as
// sibling tests expect. Best-effort, so cleanup never fails a passing test.
func returnTapeToSlot(changer *tape.Changer, slotAddr, driveAddr int, barcode tape.Barcode) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	inv, err := changer.Inventory(cleanupCtx)
	if err != nil {
		return
	}

	// The run failed before Eject.
	if len(inv.Drives) > 0 && inv.Drives[0].Loaded {
		_ = changer.Unload(cleanupCtx, slotAddr, driveAddr)

		return
	}

	// Parked in an I/O slot by Eject.
	for _, io := range inv.IOSlots {
		if io.Full && io.Barcode == barcode {
			_ = changer.Transfer(cleanupCtx, io.Address, slotAddr)

			return
		}
	}
}
