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

// TestControlWorkerAutoscaling exercises KEDA scale-to-zero (#113, #115, #116)
// with the shared Deployment scaled to 0 so the KEDA-spawned worker is the sole
// poller. Each subtest starts and ends at zero running control pods.
func TestControlWorkerAutoscaling(t *testing.T) {
	h := requireHarness(t)

	// The always-on Deployment would take every task and hide scale-from-zero.
	h.scaleControlDeployment(t, 0)
	t.Cleanup(h.restoreControlReplicas)

	h.installScaledJobWorker(t)

	t.Run("ScaleUpSelfExitRespawn", func(t *testing.T) {
		testScaleUpSelfExitRespawn(t, h)
	})

	t.Run("FailureAlertFromZero", func(t *testing.T) {
		testFailureAlertFromZero(t, h)
	})
}

// podPollTimeout comfortably covers pollingInterval + idle exit + start + replay.
const podPollTimeout = 90 * time.Second

// requireControlPods asserts the ScaledJob release settles at want running pods.
func requireControlPods(t *testing.T, h *e2eHarness, want int, msg string) {
	t.Helper()

	require.Eventuallyf(t, func() bool {
		return h.runningControlPods(t, scaledJobRelease) == want
	}, podPollTimeout, 2*time.Second, "%s (want %d running control pod(s))", msg, want)
}

// requireControlPodsAtLeast asserts at least one running control pod appears.
func requireControlPodsAtLeast(t *testing.T, h *e2eHarness, min int, msg string) {
	t.Helper()

	require.Eventuallyf(t, func() bool {
		return h.runningControlPods(t, scaledJobRelease) >= min
	}, podPollTimeout, 2*time.Second, "%s (want >= %d running control pod(s))", msg, min)
}

// testScaleUpSelfExitRespawn (AC3, AC4): an I/O-station-overflow run scales up,
// self-exits to zero during the Eject pause, and respawns on the durable
// `tapectl resume` signal to finish.
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

	// One more copy than I/O slots, so the final eject overflows and pauses.
	// Slots 20+ are clear of every other test.
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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	cfg.Library.IOWaitTimeoutSeconds = &ioWait
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 20*time.Minute)
	defer cancel()

	requireControlPods(t, h, 0, "control worker must start scaled to zero")

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	requireControlPodsAtLeast(t, h, 1, "submitting a run must scale a control worker up from zero")

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

	// The paused run leaves the control queue idle, so the worker exits mid-run.
	requireControlPods(t, h, 0, "the control worker must self-exit to zero while the run is paused")

	// Simulate the operator freeing an I/O slot.
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

	// The durable signal lands with no worker running; KEDA respawns one.
	h.resumeRun(t)

	requireControlPodsAtLeast(t, h, 1, "resuming a paused run must respawn a control worker from zero")

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, &result),
		"workflow must complete after the resume signal")

	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete in order")

	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 1, "the report is delivered (report-only delivery, SPEC §5)")

	report := extractPDFText(t, findUpload(t, uploads, "report.pdf"))
	assert.Contains(t, report, temporalRunID(t, temporalClient), "report must name the run ID")

	for _, barcode := range fixture.barcodes {
		assert.Containsf(t, report, string(barcode), "report must list tape barcode %s", barcode)
	}

	requireControlPods(t, h, 0, "the control worker must return to zero after the run completes")
}

// testFailureAlertFromZero asserts a Verify-fault failure starting from zero
// workers still delivers its failure alert via a KEDA-spawned worker (AC5).
func testFailureAlertFromZero(t *testing.T, h *e2eHarness) {
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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 8*time.Minute)
	defer cancel()

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	// Watch from submission: a fresh worker can pass Verify within the poll below.
	corruptAfterPrepare(t, temporalClient, h.stagingHostDir, 4*time.Minute)

	requireControlPodsAtLeast(t, h, 1, "submitting the run must scale a control worker up from zero")

	err := temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, new(backup.Result))
	require.Error(t, err, "run must fail after the injected checksum fault")
	assert.Contains(t, err.Error(), backup.PhaseVerify, "failure must be attributed to the Verify phase")

	// AC5.
	assertFailureAlert(t, h, backupWorkflowID, backup.PhaseVerify)
}

// TestControlWorkerMultipleReplicas asserts two concurrent control pollers don't
// double-process the singleton run. A two-replica Deployment is used because KEDA
// can't be reliably pushed past one Job for a single-workflow backlog.
func TestControlWorkerMultipleReplicas(t *testing.T) {
	h := requireHarness(t)

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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
	defer cancel()

	require.Equal(t, 2, h.runningControlPods(t, helmRelease), "two control workers must be polling the control queue")

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, &result),
		"workflow must complete successfully with two control workers")

	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete in order")

	// Exactly one report (report-only delivery, SPEC §5), never a duplicate.
	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 1, "exactly one report is delivered — no duplicate from the second worker")

	report := extractPDFText(t, findUpload(t, uploads, "report.pdf"))
	assert.Contains(t, report, temporalRunID(t, temporalClient), "report must name the run ID")
	assert.Contains(t, report, string(fixture.barcode), "report must list the tape barcode")
}
