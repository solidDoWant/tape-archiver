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

// TestEjectAutoResumeOnAccess drives the Eject phase through real Temporal and
// workers against mhvtl to cover auto-resume (#85 AC5): the SG_IO changer reports
// the import/export ACCESS bit, so clearing one slot resumes the paused export
// with no OperatorResumeSignal. Only Eject runs, so no tape writes are needed.
func TestEjectAutoResumeOnAccess(t *testing.T) {
	requireTemporalAddress(t)
	testutil.SkipIfMhvtlUnavailable(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))
	ctx := t.Context()

	inv, err := changer.Inventory(ctx)
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least one drive required")
	require.GreaterOrEqualf(t, len(inv.IOSlots), 2, "need at least two I/O slots to test overflow")

	// Without the ACCESS bit auto-resume can never fire.
	require.True(t, inv.IOAccessReported, "SG_IO changer must report the import/export ACCESS bit")

	ioSlots := len(inv.IOSlots)
	for _, io := range inv.IOSlots {
		require.Falsef(t, io.Full, "I/O slot %d must start empty", io.Address)
	}

	// One more tape than I/O slots, so the last waits for the operator.
	tapeCount := ioSlots + 1

	// High slots, clear of the sibling tests' slots 0-7 in the shared library.
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
			Barcode:    slot.Barcode,
			DriveIndex: 0,
			SourceSlot: slot.Address,
		})
		barcodes = append(barcodes, slot.Barcode)
	}

	// Return every tape to its home slot for the sibling tests.
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
	// The auto-resume poll loop child workflow (#168).
	controlWorker.RegisterWorkflow(ioStationWaitWorkflow)
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

	// Check CurrentPauseQuery (#250) with real barcodes. The station filling and
	// the query-visible pause are updated by different actors, so poll the query
	// rather than assume it is consistent the instant the changer looks full.
	var currentPause CurrentPause

	require.Eventuallyf(t, func() bool {
		pauseResponse, queryErr := temporalClient.QueryWorkflow(runCtx, run.GetID(), run.GetRunID(), CurrentPauseQuery)
		if queryErr != nil {
			return false
		}

		if err := pauseResponse.Get(&currentPause); err != nil {
			return false
		}

		return currentPause.Kind == PauseEject
	}, 30*time.Second, time.Second, "CurrentPauseQuery must report the Eject pause once the I/O station is full")

	assert.Len(t, currentPause.AffectedTapes, ioSlots, "AffectedTapes must list every tape actually sitting in the I/O station")

	pauseCur, err := changer.Inventory(runCtx)
	require.NoError(t, err, "inventory during pause, to cross-check CurrentPauseQuery's AffectedTapes")

	var inIOAtPause []string

	for _, io := range pauseCur.IOSlots {
		if io.Full {
			inIOAtPause = append(inIOAtPause, string(io.Barcode))
		}
	}

	assert.ElementsMatch(t, inIOAtPause, currentPause.AffectedTapes,
		"CurrentPauseQuery's AffectedTapes must match the real barcodes physically in the I/O station")

	// Simulate the operator removing one exported tape; that alone must resume.
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
