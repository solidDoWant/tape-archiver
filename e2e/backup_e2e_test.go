//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kdomanski/iso9660"
	"github.com/ledongthuc/pdf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// TestBackupEndToEnd_FullRun drives the whole backup workflow through the
// production-shaped deployment — control worker in kind (Helm chart + image),
// data worker as its OCI container on the host, real dev Temporal, mhvtl, and a
// real ZFS snapshot — then inspects the delivered artifacts.
//
//   - AC1: the run completes with all ten phases in order and delivers exactly two
//     artifacts (report + compressed recovery ISO).
//   - AC2: the delivered PDF report carries the run ID, the archive manifest, the
//     tape barcode, and the age private identity.
//   - AC3: the delivered recovery ISO contains age, par2, and zstd, each a valid
//     executable that actually runs.
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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
	defer cancel()

	h.submitRun(t, cfg, runID)
	terminateOnCleanup(t, temporalClient, runID)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, runID, "").Get(runCtx, &result),
		"workflow must complete successfully")

	// AC1: every phase ran to completion, in order.
	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete in order")

	// AC1: the run delivered exactly the report and the compressed recovery ISO.
	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 2, "report and recovery ISO must both be delivered")

	report := findUpload(t, uploads, "report.pdf")
	iso := findUpload(t, uploads, "recovery.iso.zst")

	assertReportContents(t, report, runID, string(fixture.barcode))
	assertRecoveryBinariesRun(t, iso)
}

// TestBackupEndToEnd_MultipleDriveSets drives a run whose copy count exceeds the
// library's drive count through the full deployment, so the tape path writes the
// copies across successive drive-sets (issue #66). With a single drive and two
// copies of one logical tape, the run writes two physical tapes in two drive-sets,
// one after the other, and delivers a report naming both. Skips (via the fixture
// and harness) when mhvtl, the ZFS pool, or the deployment is unavailable.
func TestBackupEndToEnd_MultipleDriveSets(t *testing.T) {
	h := requireHarness(t)

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	// Two blank tapes on a single drive; Copies=2 makes the run span two drive-sets.
	// Slots 8 and 9 avoid the FullRun (2), verify-fault (2), and k8s-source (3) tests.
	fixture := prepareBlankTapesAt(t, 8, 9)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-backup-multiset-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:     2,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")
	require.Greater(t, cfg.Copies, len(cfg.Library.Drives), "this test must exercise more copies than drives")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
	defer cancel()

	h.submitRun(t, cfg, runID)
	terminateOnCleanup(t, temporalClient, runID)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, runID, "").Get(runCtx, &result),
		"workflow must complete successfully")

	// Every phase ran to completion, in order — the drive-set loop still reports
	// Load, Write, and Eject once each.
	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete in order")

	// The run delivered the report and the compressed recovery ISO.
	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 2, "report and recovery ISO must both be delivered")

	// Both physical copies were written across the two drive-sets, so the report
	// lists both tape barcodes.
	report := extractPDFText(t, findUpload(t, uploads, "report.pdf"))
	assert.Contains(t, report, runID, "report must name the run ID")

	for _, barcode := range fixture.barcodes {
		assert.Containsf(t, report, string(barcode), "report must list tape barcode %s", barcode)
	}
}

// TestBackupEndToEnd_IOStationOverflow drives the operator-in-the-loop Eject phase
// through the full deployment (issue #67, and the user's request that the e2e
// suite cover this path). It runs a backup whose copies outnumber the library's
// I/O slots, so the Eject phase fills the station and pauses; the test then
// simulates the operator removing one exported tape and resumes the run through
// `tapectl resume`, asserting it completes and delivers a report naming every
// tape. mhvtl does not report the import/export access bit, so this exercises the
// signalled-resume path end to end.
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

	// One more physical copy than the library has I/O slots, so the final eject
	// overflows the station and pauses. Slots 12+ avoid the FullRun (2),
	// verify-fault (2), k8s-source (3), and MultipleDriveSets (8, 9) tests.
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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	cfg.Library.IOWaitTimeoutSeconds = &ioWait
	require.NoError(t, cfg.Validate(), "run config must be valid")
	require.Greaterf(t, copies, ioSlots, "run must write more tapes (%d) than the %d I/O slots", copies, ioSlots)

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 20*time.Minute)
	defer cancel()

	h.submitRun(t, cfg, runID)
	terminateOnCleanup(t, temporalClient, runID)

	// The run exports ioSlots tapes into the station, then the next eject fills it
	// and pauses with the last tape unloaded back to its source slot (drive empty).
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

	// Resume through the operator CLI; the run exports the final tape and completes.
	h.resumeRun(t, runID)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, runID, "").Get(runCtx, &result),
		"workflow must complete after the resume signal")

	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete in order")

	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 2, "report and recovery ISO must both be delivered")

	report := extractPDFText(t, findUpload(t, uploads, "report.pdf"))
	assert.Contains(t, report, runID, "report must name the run ID")

	for _, barcode := range fixture.barcodes {
		assert.Containsf(t, report, string(barcode), "report must list tape barcode %s", barcode)
	}
}

// findUpload returns the captured upload whose filename matches, failing if it is
// absent.
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

// assertReportContents parses the delivered PDF and asserts it carries the run
// ID, the archive manifest (a PAR2 recovery-set file name), the tape barcode, and
// the age private identity (AC2).
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

// assertRecoveryBinariesRun decompresses the delivered recovery ISO, extracts the
// recovery binaries, and runs each one to prove it is present and executable (AC3,
// strong reading: the ISO carries the real static binaries and they actually run).
func assertRecoveryBinariesRun(t *testing.T, isoZst []byte) {
	t.Helper()

	files := readISO(t, decompressZstd(t, isoZst))

	for _, name := range []string{"age", "par2", "zstd"} {
		binary, ok := files["bin/"+name]
		require.Truef(t, ok, "recovery ISO must contain bin/%s (have %v)", name, slices.Collect(maps.Keys(files)))

		binPath := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(binPath, binary, 0o755))

		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)

		out, err := exec.CommandContext(ctx, binPath, "--version").CombinedOutput()

		cancel()

		require.NoErrorf(t, err, "recovery binary %s must run: %s", name, out)
		assert.NotEmptyf(t, out, "recovery binary %s --version must print a version", name)
	}
}

// decompressZstd inflates a .zst blob by shelling to the zstd CLI (pkg/archive
// only compresses).
func decompressZstd(t *testing.T, compressed []byte) []byte {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "in.zst")
	dst := filepath.Join(dir, "out")

	require.NoError(t, os.WriteFile(src, compressed, 0o600))

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "zstd", "-d", "-f", "-o", dst, src).CombinedOutput()
	require.NoErrorf(t, err, "zstd -d: %s", out)

	data, err := os.ReadFile(dst)
	require.NoError(t, err)

	return data
}

// readISO reads an ISO 9660 image into a name→content map (Rock Ridge names, e.g.
// "bin/age"), walking the directory tree.
func readISO(t *testing.T, image []byte) map[string][]byte {
	t.Helper()

	img, err := iso9660.OpenImage(bytes.NewReader(image))
	require.NoError(t, err)

	root, err := img.RootDir()
	require.NoError(t, err)

	files := make(map[string][]byte)

	var walk func(dir *iso9660.File, prefix string)

	walk = func(dir *iso9660.File, prefix string) {
		children, err := dir.GetChildren()
		require.NoError(t, err)

		for _, child := range children {
			full := path.Join(prefix, child.Name())

			if child.IsDir() {
				walk(child, full)

				continue
			}

			data, err := io.ReadAll(child.Reader())
			require.NoError(t, err)

			files[full] = data
		}
	}

	walk(root, "")

	return files
}
