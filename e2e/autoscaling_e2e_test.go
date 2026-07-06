//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// TestControlWorkerAutoscaling exercises the opt-in KEDA scale-to-zero control-worker
// path (parent #113; blockers #115 idle-exit, #116 ScaledJob chart) end to end. It
// removes the shared always-on Deployment worker from the control queue (scale to 0)
// so a KEDA-spawned worker is the sole poller, then installs the worker in its
// ScaledJob shape with a fast-cycle overlay (short WORKER_IDLE_EXIT_AFTER, low KEDA
// pollingInterval) so the full 0 -> 1 -> 0 -> 1 cycle is observable in seconds.
//
// Both subtests share that setup; each starts from and returns to zero running control
// pods, and each submits its run under the singleton workflow ID and terminates it on
// cleanup. The shared Deployment is only replica-toggled — restored to one replica
// afterwards — so the rest of the suite still exercises the baseline Deployment shape.
func TestControlWorkerAutoscaling(t *testing.T) {
	h := requireHarness(t)

	// Single-poller isolation: the always-on Deployment worker would grab every
	// workflow task and make scale-from-zero unobservable, so take it off the control
	// queue for the duration and restore it afterwards.
	h.scaleControlDeployment(t, 0)
	t.Cleanup(h.restoreControlReplicas)

	// Install the control worker in its ScaledJob shape (fast scale-to-zero overlay).
	h.installScaledJobWorker(t)

	t.Run("ScaleUpSelfExitRespawn", func(t *testing.T) {
		testScaleUpSelfExitRespawn(t, h)
	})

	t.Run("FailureAlertFromZero", func(t *testing.T) {
		testFailureAlertFromZero(t, h)
	})
}

// podPollTimeout bounds the wait for a scale transition. KEDA's pollingInterval (5s)
// plus WORKER_IDLE_EXIT_AFTER (20s) plus image start and workflow replay all fit
// comfortably inside it, so a genuine regression (a transition that never happens)
// fails rather than flakes.
const podPollTimeout = 90 * time.Second

// requireControlPods asserts the ScaledJob release settles at the wanted number of
// running control pods within podPollTimeout — the load-bearing scale assertions.
func requireControlPods(t *testing.T, h *e2eHarness, want int, msg string) {
	t.Helper()

	require.Eventuallyf(t, func() bool {
		return h.runningControlPods(t, scaledJobRelease) == want
	}, podPollTimeout, 2*time.Second, "%s (want %d running control pod(s))", msg, want)
}

// requireControlPodsAtLeast asserts at least one running control pod appears within
// podPollTimeout, used for the scale-up and respawn transitions where the exact count
// is bounded by maxReplicaCount but only "a worker came back" matters.
func requireControlPodsAtLeast(t *testing.T, h *e2eHarness, min int, msg string) {
	t.Helper()

	require.Eventuallyf(t, func() bool {
		return h.runningControlPods(t, scaledJobRelease) >= min
	}, podPollTimeout, 2*time.Second, "%s (want >= %d running control pod(s))", msg, min)
}

// testScaleUpSelfExitRespawn drives the full autoscaling cycle through an
// I/O-station-overflow run (AC3 + AC4): idle at zero; a submitted run scales a worker
// up; the Eject phase fills the station and pauses, the control queue goes idle, and
// the worker self-exits back to zero *during* the pause; `tapectl resume` (a durable
// signal, delivered with no worker running) enqueues a workflow task that respawns a
// worker, which replays and drives the run to completion; then it returns to zero.
func testScaleUpSelfExitRespawn(t *testing.T, h *e2eHarness) {
	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqualf(t, len(inv.IOSlots), 2, "need at least two I/O slots to overflow")

	ioSlots := len(inv.IOSlots)
	for _, io := range inv.IOSlots {
		require.Falsef(t, io.Full, "I/O slot %d must start empty", io.Address)
	}

	// One more physical copy than the library has I/O slots, so the final eject
	// overflows the station and pauses. Slots 20+ are clear of every other test.
	copies := ioSlots + 1

	slotIndexes := make([]int, copies)
	for i := range slotIndexes {
		slotIndexes[i] = 20 + i
	}

	fixture := prepareBlankTapesAt(t, slotIndexes...)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-autoscale-overflow-%d", time.Now().UnixNano())

	ioWait := 600

	cfg := config.Config{
		Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:     copies,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	cfg.Library.IOWaitTimeoutSeconds = &ioWait
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 20*time.Minute)
	defer cancel()

	// No run in progress: no control worker is running.
	requireControlPods(t, h, 0, "control worker must start scaled to zero")

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	// Scale-up: KEDA sees the workflow task on the control backlog and spawns a worker.
	requireControlPodsAtLeast(t, h, 1, "submitting a run must scale a control worker up from zero")

	// The run exports ioSlots tapes into the station, then the next eject fills it and
	// pauses with the last tape unloaded back to its source slot (drive empty).
	lastBarcode := fixture.barcodes[copies-1]
	lastSlot := fixture.library.BlankSlots[copies-1]

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

		lastParked := false

		for _, storage := range cur.Slots {
			if storage.Address == lastSlot && storage.Full && storage.Barcode == lastBarcode {
				lastParked = true
			}
		}

		return full == ioSlots && lastParked
	}, 15*time.Minute, 2*time.Second, "the Eject phase must fill the I/O station and pause")

	// Self-exit during the pause: the run is parked awaiting the resume signal, so the
	// control queue goes idle and the worker drains and exits, scaling back to zero
	// mid-run — the load-bearing self-exit that makes scale-to-zero worthwhile.
	requireControlPods(t, h, 0, "the control worker must self-exit to zero while the run is paused")

	// Simulate the operator removing one exported tape: move the first exported tape
	// from its I/O slot back to its source storage slot, freeing an I/O slot.
	cur, err := changer.Inventory(runCtx)
	require.NoError(t, err, "inventory at pause")

	firstBarcode := fixture.barcodes[0]
	firstSlot := fixture.library.BlankSlots[0]
	ioAddr := -1

	for _, io := range cur.IOSlots {
		if io.Full && io.Barcode == firstBarcode {
			ioAddr = io.Address

			break
		}
	}

	require.NotEqualf(t, -1, ioAddr, "exported tape %s must be in an I/O slot at the pause", firstBarcode)
	require.NoError(t, changer.Transfer(runCtx, ioAddr, firstSlot), "operator clears one I/O slot")

	// Resume through the operator CLI. The signal is durable and lands with no worker
	// running; it enqueues a control workflow task that KEDA respawns a worker to serve.
	h.resumeRun(t)

	// Respawn: a worker comes back from zero to finish the run.
	requireControlPodsAtLeast(t, h, 1, "resuming a paused run must respawn a control worker from zero")

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, &result),
		"workflow must complete after the resume signal")

	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete in order")

	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 2, "report and recovery ISO must both be delivered")

	report := extractPDFText(t, findUpload(t, uploads, "report.pdf"))
	assert.Contains(t, report, backupWorkflowID, "report must name the run ID")

	for _, barcode := range fixture.barcodes {
		assert.Containsf(t, report, string(barcode), "report must list tape barcode %s", barcode)
	}

	// Once the completed run drains the control queue, the worker self-exits again.
	requireControlPods(t, h, 0, "the control worker must return to zero after the run completes")
}

// testFailureAlertFromZero asserts a run that fails while the control worker was
// scaled to zero still delivers its failure alert to DISCORD_FAILURE_WEBHOOK_URL via a
// KEDA-spawned worker (AC5). With the shared Deployment off the control queue, any
// worker that runs — including the one that posts the failure alert — was spawned by
// KEDA from zero. It reuses the Verify-fault injection: a staged slice is corrupted
// after Prepare, so the run fails at Verify and the workflow's failure path fires.
func testFailureAlertFromZero(t *testing.T, h *e2eHarness) {
	// Start from a genuine scaled-to-zero state so the alert is provably KEDA-spawned.
	requireControlPods(t, h, 0, "control worker must be scaled to zero before the failing run")

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	fixture := prepareBlankTapeAt(t, 24)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-autoscale-failure-%d", time.Now().UnixNano())

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

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	// KEDA must spawn a worker to drive the run from zero.
	requireControlPodsAtLeast(t, h, 1, "submitting the run must scale a control worker up from zero")

	// Corrupt one staged slice after Prepare so Verify's checksum recompute fails.
	waitForPhase(t, temporalClient, backup.PhasePrepare, 4*time.Minute)
	corruptStagedSlice(t, fmt.Sprintf("%s/%s", h.stagingHostDir, temporalRunID(t, temporalClient)))

	// The run must fail, attributed to the Verify phase.
	err := temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, new(backup.Result))
	require.Error(t, err, "run must fail after the injected checksum fault")
	assert.Contains(t, err.Error(), backup.PhaseVerify, "failure must be attributed to the Verify phase")

	// AC5: the failure alert still reaches the webhook, posted by the KEDA-spawned
	// worker — a control worker at zero does not swallow failure alerting.
	assertFailureAlert(t, h, backupWorkflowID, backup.PhaseVerify)
}

// TestControlWorkerMultipleReplicas covers the multi-worker edge case: at steady state
// a single worker polls the control queue, but two control workers must be able to
// poll it concurrently without double-processing the singleton run. It scales the
// shared Deployment to two replicas, drives a full backup, and asserts the run
// completes exactly once — exactly the two artifacts delivered, no duplicate delivery —
// which holds because Temporal serializes workflow-task processing per execution.
//
// (KEDA's backlog-driven scaler cannot be deterministically pushed past one Job for a
// single-workflow backlog, so a fixed two-replica Deployment is the reliable way to
// force two concurrent pollers.)
func TestControlWorkerMultipleReplicas(t *testing.T) {
	h := requireHarness(t)

	// Two concurrent pollers on the singleton control queue; restore to one afterwards.
	h.scaleControlDeployment(t, 2)
	t.Cleanup(h.restoreControlReplicas)

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	fixture := prepareBlankTapeAt(t, 26)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-multi-replica-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:     1,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
	defer cancel()

	// Both workers are up and polling before the run is submitted.
	require.Equal(t, 2, h.runningControlPods(t, helmRelease), "two control workers must be polling the control queue")

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, &result),
		"workflow must complete successfully with two control workers")

	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete in order")

	// Exactly-once: two concurrent workers must not double-process the run, so exactly
	// the report and the recovery ISO are delivered — never a duplicate set.
	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 2, "exactly the report and recovery ISO must be delivered — no duplicates from the second worker")

	report := extractPDFText(t, findUpload(t, uploads, "report.pdf"))
	assert.Contains(t, report, backupWorkflowID, "report must name the run ID")
	assert.Contains(t, report, string(fixture.barcode), "report must list the tape barcode")
}
