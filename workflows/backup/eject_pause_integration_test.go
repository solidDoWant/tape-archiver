//go:build integration

package backup

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// TestEjectAutoResumeOnAccess drives the operator-in-the-loop Eject phase through
// a real Temporal server and real workers against mhvtl, exercising the
// auto-resume path (issue #85 AC5): because the changer now reads element status
// via SG_IO, the import/export ACCESS bit is reported, so a paused Eject resumes
// automatically once the operator clears a slot — with no explicit
// OperatorResumeSignal.
//
// It runs a run whose written tapes outnumber the library's I/O slots so the
// Eject phase fills the station and pauses, then simulates the operator removing
// one exported tape (which frees and re-closes a slot). With the ACCESS bit now
// live on mhvtl, IOStatus.CanAutoResume becomes true and the run resumes on the
// next poll and exports the remaining tape — the signal is never sent. The
// signal- and timeout-driven resume paths remain covered by the mock-based unit
// tests in eject_pause_test.go.
//
// It drives only the Eject phase (ejectPauseTestWorkflow) so it needs no real
// tape writes: the "written" tapes are ordinary tapes parked in storage slots that
// Eject transfers straight to the I/O station. Skips when mhvtl or dev Temporal is
// absent. Driven by `make test-integration`.
func TestEjectAutoResumeOnAccess(t *testing.T) {
	requireTemporalAddress(t)
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))
	ctx := t.Context()

	inv, err := changer.Inventory(ctx)
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least one drive required")
	require.GreaterOrEqualf(t, len(inv.IOSlots), 2, "need at least two I/O slots to test overflow")

	// The changer must now report the import/export ACCESS bit; otherwise
	// auto-resume can never fire and this test could not pass.
	require.True(t, inv.IOAccessReported, "SG_IO changer must report the import/export ACCESS bit")

	ioSlots := len(inv.IOSlots)
	for _, io := range inv.IOSlots {
		require.Falsef(t, io.Full, "I/O slot %d must start empty", io.Address)
	}

	// Write one more physical tape than the library has I/O slots so the last one
	// cannot be exported until the operator clears a slot.
	tapeCount := ioSlots + 1

	// Use storage slots from a high index range the sibling integration tests
	// (slots 0–7) do not touch, so the shared mhvtl library does not collide.
	const baseIndex = 20
	require.Greaterf(t, len(inv.Slots), baseIndex+tapeCount, "need storage slots in the %d+ range", baseIndex)

	var (
		written  []WrittenTape
		barcodes []tape.Barcode
	)

	for i := 0; i < tapeCount; i++ {
		slot := inv.Slots[baseIndex+i]
		require.Truef(t, slot.Full, "storage slot index %d must hold a tape", baseIndex+i)
		require.NotEmptyf(t, slot.Barcode, "storage slot index %d tape must have a barcode", baseIndex+i)

		written = append(written, WrittenTape{
			Barcode: slot.Barcode,
			// The drive is left empty in this test; Eject transfers each tape
			// straight from its source storage slot to the I/O station.
			DriveIndex: 0,
			SourceSlot: slot.Address,
		})
		barcodes = append(barcodes, slot.Barcode)
	}

	// Restore the library on exit: move every tape back to its home storage slot so
	// the sibling tests find the expected state.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 120*time.Second)
		defer cancel()

		cur, err := changer.Inventory(cleanupCtx)
		if err != nil {
			return
		}

		for _, wt := range written {
			for _, io := range cur.IOSlots {
				if io.Full && io.Barcode == wt.Barcode {
					_ = changer.Transfer(cleanupCtx, io.Address, wt.SourceSlot)
				}
			}
		}
	})

	temporalClient := dialTemporal(t)

	controlWorker := worker.New(temporalClient, TaskQueue, worker.Options{})
	controlWorker.RegisterWorkflow(ejectPauseTestWorkflow)
	controlWorker.RegisterActivity(&FailureActivities{})
	require.NoError(t, controlWorker.Start(), "start control worker")
	t.Cleanup(controlWorker.Stop)

	dataWorker := worker.New(temporalClient, DataTaskQueue, worker.Options{})
	dataWorker.RegisterActivity(newEjectActivities())
	require.NoError(t, dataWorker.Start(), "start data worker")
	t.Cleanup(dataWorker.Stop)

	ioWait := 300

	sourceSlots := make([]int, len(written))
	for i, wt := range written {
		sourceSlots[i] = wt.SourceSlot
	}

	cfg := config.Config{
		Library: config.Library{
			Changer:              testutil.ChangerDev(t),
			Drives:               []string{testutil.Drive0Dev(t)},
			BlankSlots:           sourceSlots,
			TapeCapacityBytes:    2_500_000_000_000,
			IOWaitTimeoutSeconds: &ioWait,
		},
	}

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Minute)
	defer cancel()

	options := client.StartWorkflowOptions{
		ID:        fmt.Sprintf("e2e-eject-auto-resume-%d", time.Now().UnixNano()),
		TaskQueue: TaskQueue,
	}

	run, err := temporalClient.ExecuteWorkflow(runCtx, options, ejectPauseTestWorkflow,
		ejectPauseParams{Cfg: cfg, Written: written})
	require.NoError(t, err, "start workflow")

	// The Eject phase fills every I/O slot and pauses (one tape cannot fit).
	require.Eventuallyf(t, func() bool {
		cur, invErr := changer.Inventory(runCtx)
		if invErr != nil {
			return false
		}

		full := 0

		for _, io := range cur.IOSlots {
			if io.Full {
				full++
			}
		}

		return full == ioSlots
	}, 90*time.Second, time.Second, "the Eject phase must fill all %d I/O slots and pause", ioSlots)

	// Simulate the operator removing one exported tape, freeing an I/O slot. With
	// the ACCESS bit now reported by the SG_IO changer, this alone must resume the
	// run automatically — no OperatorResumeSignal is ever sent.
	cur, err := changer.Inventory(runCtx)
	require.NoError(t, err, "inventory at pause")

	var (
		removed   tape.Barcode
		freedFrom = -1
		homeSlot  = -1
	)

	for _, io := range cur.IOSlots {
		if io.Full {
			removed = io.Barcode
			freedFrom = io.Address

			break
		}
	}

	require.NotEmpty(t, removed, "a tape must be in an I/O slot at the pause")

	for _, wt := range written {
		if wt.Barcode == removed {
			homeSlot = wt.SourceSlot

			break
		}
	}

	require.NotEqual(t, -1, homeSlot, "the removed tape must be one of the written tapes")
	require.NoError(t, changer.Transfer(runCtx, freedFrom, homeSlot), "operator clears one I/O slot")

	// The run resumes on its own (poll interval) and completes — no signal.
	require.NoError(t, run.Get(runCtx, nil), "workflow must auto-resume and complete once a slot is freed")

	// Every written tape was exported: the ones still in I/O slots plus the one the
	// operator removed account for all of them.
	finalInv, err := changer.Inventory(runCtx)
	require.NoError(t, err, "final inventory")

	inIO := make(map[tape.Barcode]bool)

	for _, io := range finalInv.IOSlots {
		if io.Full {
			inIO[io.Barcode] = true
		}
	}

	exported := 0

	for _, barcode := range barcodes {
		if inIO[barcode] || barcode == removed {
			exported++
		}
	}

	assert.Equal(t, tapeCount, exported, "every written tape must have been exported")
}
