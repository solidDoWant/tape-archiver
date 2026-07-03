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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 8*time.Minute)
	defer cancel()

	h.submitRun(t, cfg, runID)
	terminateOnCleanup(t, temporalClient, runID)

	// Wait until Prepare has completed (its slices + recorded checksums exist on
	// disk), then corrupt one slice before Verify recomputes it. Prepare keys the
	// per-run staging subdirectory by the Temporal RunID (not the workflow ID).
	waitForPhase(t, temporalClient, runID, backup.PhasePrepare, 4*time.Minute)
	corruptStagedSlice(t, filepath.Join(h.stagingHostDir, temporalRunID(t, temporalClient, runID)))

	// The run must fail, and the failure must be attributed to the Verify phase.
	err := temporalClient.GetWorkflow(runCtx, runID, "").Get(runCtx, new(backup.Result))
	require.Error(t, err, "run must fail after the injected checksum fault")
	assert.Contains(t, err.Error(), backup.PhaseVerify, "failure must be attributed to the Verify phase")

	// AC4: the control worker's failure webhook received an alert naming this run
	// and the Verify phase.
	assertFailureAlert(t, h, runID, backup.PhaseVerify)

	// AC4: no tape was loaded or written — Load never ran, so drive 0 is still
	// empty and the blank tape is still in its storage slot.
	assertTapeUntouched(t, fixture)
}

// waitForPhase polls LastCompletedPhaseQuery until the workflow reports the given
// phase as its last completed phase (or a later data-side phase), so the caller
// can act within the window after it.
func waitForPhase(t *testing.T, c client.Client, runID, phase string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		resp, err := c.QueryWorkflow(t.Context(), runID, "", backup.LastCompletedPhaseQuery)
		if err == nil {
			var last string
			require.NoError(t, resp.Get(&last))

			if last == phase {
				return
			}

			// If we already blew past the target into Pack/PAR2, the slice still
			// exists and is corruptible before Verify; stop waiting.
			if last == backup.PhasePack || last == backup.PhaseGeneratePAR2 {
				return
			}
		}

		time.Sleep(250 * time.Millisecond)
	}

	require.Failf(t, "phase not reached", "workflow did not complete phase %q within %s", phase, timeout)
}

// corruptStagedSlice flips a byte in the middle of the largest staged slice under
// dir, preserving its size so the fault is a content (checksum) mismatch rather
// than a truncation. PAR2 files do not exist yet at this point (a later phase),
// so every regular file here is an archive slice.
func corruptStagedSlice(t *testing.T, dir string) {
	t.Helper()

	var (
		target string
		size   int64
	)

	require.Eventually(t, func() bool {
		target, size = "", 0

		_ = filepath.WalkDir(dir, func(pathName string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // tolerate races with the writing worker
			}

			info, statErr := d.Info()
			if statErr == nil && info.Size() > size {
				target, size = pathName, info.Size()
			}

			return nil
		})

		return size > 0
	}, 60*time.Second, 250*time.Millisecond, "a staged slice must appear under %s", dir)

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	data[len(data)/2] ^= 0xff
	require.NoError(t, os.WriteFile(target, data, 0o600))

	t.Logf("corrupted staged slice %s (%d bytes)", target, size)
}

// assertFailureAlert asserts the mock failure webhook received exactly the alert
// for this run, naming the run ID and the failing phase (the SendFailure format
// is "Backup run <id> failed in phase <phase>: <err>").
func assertFailureAlert(t *testing.T, h *e2eHarness, runID, phase string) {
	t.Helper()

	require.Eventually(t, func() bool {
		for _, msg := range h.rec.failureMessages() {
			if strings.Contains(msg, runID) && strings.Contains(msg, phase) {
				return true
			}
		}

		return false
	}, 60*time.Second, 500*time.Millisecond, "failure webhook must receive an alert for run %s naming phase %s", runID, phase)
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
