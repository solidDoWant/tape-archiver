//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ledongthuc/pdf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// TestBackupEndToEnd_FullRun runs a full backup through the deployment and
// asserts all phases run in order (AC1), exactly the report is delivered
// (SPEC §5), and the report carries the expected contents (AC2).
func TestBackupEndToEnd_FullRun(t *testing.T) {
	h := requireHarness(t)

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	fixture := prepareBlankTape(t)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-backup-%d", time.Now().UnixNano())

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

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, &result),
		"workflow must complete successfully")

	// AC1.
	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete in order")

	// AC1: report-only delivery (SPEC §5).
	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 1, "the report is delivered (report-only delivery, SPEC §5)")

	report := findUpload(t, uploads, "report.pdf")

	assertReportContents(t, report, temporalRunID(t, temporalClient), string(fixture.barcode))

	// Issue #333: bulk staging is reclaimed but the report survives on disk for
	// disc verification and re-burns.
	assertStagingCleaned(t, filepath.Join(h.stagingHostDir, temporalRunID(t, temporalClient)))
}

// TestBackupEndToEnd_MultipleDriveSets writes two copies on one drive, i.e. two
// successive drive-sets (issue #66), and expects both barcodes in the report.
func TestBackupEndToEnd_MultipleDriveSets(t *testing.T) {
	h := requireHarness(t)

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	// Slots 8 and 9 avoid the other tests' slots.
	fixture := prepareBlankTapesAt(t, 8, 9)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-backup-multiset-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:     2,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")
	require.Greater(t, cfg.Copies, len(cfg.Library.Drives), "this test must exercise more copies than drives")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
	defer cancel()

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, &result),
		"workflow must complete successfully")

	// The drive-set loop still reports Load, Write and Eject once each.
	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete in order")

	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 1, "the report is delivered (report-only delivery, SPEC §5)")

	report := extractPDFText(t, findUpload(t, uploads, "report.pdf"))
	assert.Contains(t, report, temporalRunID(t, temporalClient), "report must name the run ID")

	for _, barcode := range fixture.barcodes {
		assert.Containsf(t, report, string(barcode), "report must list tape barcode %s", barcode)
	}
}

// TestBackupEndToEnd_IOStationOverflow overflows the I/O station so Eject
// pauses, frees a slot, and resumes via `tapectl resume` (issue #67). mhvtl
// doesn't report the access bit, so this covers the signalled-resume path.
func TestBackupEndToEnd_IOStationOverflow(t *testing.T) {
	h := requireHarness(t)

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
	// Slots 12+ avoid the other tests' slots.
	copies := ioSlots + 1

	slotIndexes := make([]int, copies)
	for i := range slotIndexes {
		slotIndexes[i] = 12 + i
	}

	fixture := prepareBlankTapesAt(t, slotIndexes...)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-backup-overflow-%d", time.Now().UnixNano())

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
	require.Greaterf(t, copies, ioSlots, "run must write more tapes (%d) than the %d I/O slots", copies, ioSlots)

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 20*time.Minute)
	defer cancel()

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

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

	h.resumeRun(t)

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
}

// stagingRetainedArtifacts duplicates workflows/backup's retainedRunArtifacts.
var stagingRetainedArtifacts = []string{"report.pdf", "recovery.iso", "disc-manifest.sha256"}

// assertStagingCleaned asserts no bulk staging data remains while report.pdf
// survives on disk.
func assertStagingCleaned(t *testing.T, runDir string) {
	t.Helper()

	var leftovers []string

	err := filepath.WalkDir(runDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		for _, retained := range stagingRetainedArtifacts {
			if entry.Name() == retained {
				return nil
			}
		}

		leftovers = append(leftovers, path)

		return nil
	})
	require.NoError(t, err, "read run staging directory %s", runDir)

	assert.Empty(t, leftovers, "no staged bulk data may survive a completed run")
	assert.FileExists(t, filepath.Join(runDir, "report.pdf"), "the report survives cleanup on disk")
}

// findUpload returns the upload named filename, failing if absent.
func findUpload(t *testing.T, uploads []upload, filename string) []byte {
	t.Helper()

	for _, u := range uploads {
		if u.filename == filename {
			return u.data
		}
	}

	require.Failf(t, "missing delivered artifact", "no upload named %q (got %v)", filename, uploadNames(uploads))

	return nil
}

func uploadNames(uploads []upload) []string {
	names := make([]string, len(uploads))
	for i, u := range uploads {
		names[i] = u.filename
	}

	return names
}

// assertReportContents asserts the PDF carries the run ID, manifest, barcode and
// age identity (AC2). runID is the execution's run id, not the singleton
// workflow ID (issue #340).
func assertReportContents(t *testing.T, reportPDF []byte, runID, barcode string) {
	t.Helper()

	report := extractPDFText(t, reportPDF)

	assert.Contains(t, report, runID, "report must name the run ID")
	assert.Contains(t, report, barcode, "report must list the tape barcode")
	assert.Contains(t, report, "AGE-SECRET-KEY", "report must embed the age private identity for escrow")
	assert.Contains(t, report, ".par2", "report manifest must list the PAR2 recovery-set files")
}

// extractPDFText returns the plain text of a delivered PDF report.
func extractPDFText(t *testing.T, reportPDF []byte) string {
	t.Helper()

	reader, err := pdf.NewReader(bytes.NewReader(reportPDF), int64(len(reportPDF)))
	require.NoError(t, err, "delivered report is not a valid PDF")

	plain, err := reader.GetPlainText()
	require.NoError(t, err)

	text, err := io.ReadAll(plain)
	require.NoError(t, err)

	return string(text)
}
