//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// TestBackupVerifyFault_NoTapeTouched injects a real disk-corruption fault after
// the Prepare phase — flipping a byte in a staged slice — and asserts the
// workflow fails at Verify, the control worker's failure webhook fires, and the
// tape was never loaded or written (AC4). Corruption (rather than a tiny tape
// capacity) is required because Pack rejects an over-capacity archive before
// Verify ever runs; a checksum mismatch is the fault Verify is designed to catch.
func TestBackupVerifyFault_NoTapeTouched(t *testing.T) {
	h := requireHarness(t)

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	fixture := prepareBlankTape(t)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-verifyfault-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:     1,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 8*time.Minute)
	defer cancel()

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	// Once Prepare has completed (its slices + recorded checksums exist on disk),
	// corrupt one slice before Verify recomputes it.
	corruptAfterPrepare(t, temporalClient, h.stagingHostDir, 4*time.Minute)

	// The run must fail, and the failure must be attributed to the Verify phase.
	err := temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, new(backup.Result))
	require.Error(t, err, "run must fail after the injected checksum fault")
	assert.Contains(t, err.Error(), backup.PhaseVerify, "failure must be attributed to the Verify phase")

	// AC4: the control worker's failure webhook received an alert naming the run
	// (the singleton workflow ID) and the Verify phase.
	assertFailureAlert(t, h, backupWorkflowID, backup.PhaseVerify)

	// AC4: no tape was loaded or written — Load never ran, so drive 0 is still
	// empty and the blank tape is still in its storage slot.
	assertTapeUntouched(t, fixture)
}

// corruptAfterPrepare waits for the run's PrepareArchives activity to complete,
// then immediately flips a byte in a staged slice so Verify's checksum recompute
// fails. Prepare -> GeneratePAR2 -> Verify can finish in well under a second, so
// completion is detected by long-polling the run's history (pushed by Temporal as
// the event is recorded) rather than by polling a query, which needs a worker
// round trip per poll and routinely missed the window.
func corruptAfterPrepare(t *testing.T, c client.Client, stagingHostDir string, timeout time.Duration) {
	t.Helper()

	runID := temporalRunID(t, c)

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	activityTypes := map[int64]string{}
	iterator := c.GetWorkflowHistory(ctx, backupWorkflowID, runID, true, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)

	for iterator.HasNext() {
		event, err := iterator.Next()
		require.NoErrorf(t, err, "long-poll history of run %s for PrepareArchives completion", runID)

		if attrs := event.GetActivityTaskScheduledEventAttributes(); attrs != nil {
			activityTypes[event.GetEventId()] = attrs.GetActivityType().GetName()
		}

		if attrs := event.GetActivityTaskCompletedEventAttributes(); attrs != nil &&
			activityTypes[attrs.GetScheduledEventId()] == "PrepareArchives" {
			corruptStagedSlice(t, filepath.Join(stagingHostDir, runID))

			return
		}
	}

	require.Failf(t, "PrepareArchives never completed", "run %s closed or %s elapsed before PrepareArchives completed", runID, timeout)
}

// corruptStagedSlice flips one byte in the middle of the largest archive slice
// (a non-PAR2 file in a per-archive subdirectory) under dir, in place and
// preserving its size, so the fault is a content (checksum) mismatch rather than
// a truncation. Top-level files such as report.pdf exist only after Verify, so
// they are never picked.
func corruptStagedSlice(t *testing.T, dir string) {
	t.Helper()

	var (
		target string
		size   int64
	)

	_ = filepath.WalkDir(dir, func(pathName string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Dir(pathName) == dir || strings.HasSuffix(pathName, ".par2") {
			return nil //nolint:nilerr // tolerate races with the writing worker
		}

		info, statErr := d.Info()
		if statErr == nil && info.Size() > size {
			target, size = pathName, info.Size()
		}

		return nil
	})
	require.Positivef(t, size, "a staged slice must exist under %s once Prepare completed", dir)

	file, err := os.OpenFile(target, os.O_RDWR, 0)
	require.NoError(t, err)

	defer func() { require.NoError(t, file.Close()) }()

	middle := make([]byte, 1)
	_, err = file.ReadAt(middle, size/2)
	require.NoError(t, err)

	middle[0] ^= 0xff
	_, err = file.WriteAt(middle, size/2)
	require.NoError(t, err)

	t.Logf("corrupted staged slice %s (%d bytes)", target, size)
}

// assertFailureAlert asserts the mock failure webhook received exactly the alert
// for this run, naming the run ID and the failing phase (the SendFailure format
// is "Backup run <id> failed in phase <phase>: <err>").
func assertFailureAlert(t *testing.T, h *e2eHarness, workflowID, phase string) {
	t.Helper()

	require.Eventually(t, func() bool {
		for _, msg := range h.rec.failureMessages() {
			if strings.Contains(msg, workflowID) && strings.Contains(msg, phase) {
				return true
			}
		}

		return false
	}, 60*time.Second, 500*time.Millisecond, "failure webhook must receive an alert for run %s naming phase %s", workflowID, phase)
}

// assertTapeUntouched asserts drive 0 is empty and the fixture's tape is still in
// its storage slot — proof that Load never ran and nothing was written.
func assertTapeUntouched(t *testing.T, fixture tapeFixture) {
	t.Helper()

	inv, err := fixture.changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")

	require.GreaterOrEqual(t, len(inv.Drives), 1)
	assert.False(t, inv.Drives[0].Loaded, "drive 0 must be empty — no tape was loaded")

	var found tape.StorageElement

	for _, slot := range inv.Slots {
		if slot.Address == fixture.slotAddr {
			found = slot

			break
		}
	}

	assert.True(t, found.Full, "the blank tape must still be in its storage slot")
	assert.Equal(t, fixture.barcode, found.Barcode, "the same tape must still be in the slot")
}
