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

// TestBackupVerifyFault_NoTapeTouched corrupts a staged slice after Prepare and
// asserts the run fails at Verify, alerts, and never touches the tape (AC4).
// Corruption is used because Pack rejects over-capacity archives before Verify.
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

	corruptAfterPrepare(t, temporalClient, h.stagingHostDir, 4*time.Minute)

	err := temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, new(backup.Result))
	require.Error(t, err, "run must fail after the injected checksum fault")
	assert.Contains(t, err.Error(), backup.PhaseVerify, "failure must be attributed to the Verify phase")

	// AC4.
	assertFailureAlert(t, h, backupWorkflowID, backup.PhaseVerify)

	// AC4: Load never ran.
	assertTapeUntouched(t, fixture)
}

// corruptAfterPrepare flips a staged byte as soon as PrepareArchives completes.
// It long-polls history because Prepare -> Verify can take under a second and
// query polling routinely missed the window.
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

// corruptStagedSlice flips a byte mid-way through the largest archive slice,
// preserving size, so the fault is a checksum mismatch rather than truncation.
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

// assertFailureAlert asserts exactly one alert naming the run and failing phase.
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

// assertTapeUntouched asserts drive 0 is empty and the tape is still in its slot.
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
