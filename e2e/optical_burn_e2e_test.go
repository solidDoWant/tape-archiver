//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/optical"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// Optical Burn phase e2e (#98, SPEC §10), using loop-device pseudo-discs plus an
// opt-in real-hardware burn gated on OPTICAL_BURN_DEV.

// TestBackupEndToEnd_OpticalBurn burns two copies on two loop burners (issue #104
// AC1). VerifyDisc runs with MaximumAttempts=1, so completion means every copy
// verified.
func TestBackupEndToEnd_OpticalBurn(t *testing.T) {
	h := requireHarness(t)
	requireOpticalDiscs(t, h)

	drives := h.opticalDevices[:2]
	for _, drive := range drives {
		blankOpticalDisc(t, drive)
	}

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	fixture := prepareBlankTapeAt(t, 4)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-optical-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:     1,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery: config.Delivery{
			WebhookURL:  h.deliveryURL(runID),
			OpticalBurn: &config.OpticalBurn{Drives: drives, Copies: len(drives)},
		},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")
	require.True(t, cfg.Delivery.OpticalBurn.Enabled(), "optical burning must be enabled")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 15*time.Minute)
	defer cancel()

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, &result),
		"workflow must complete successfully")

	assert.Equal(t, orderedPhases, result.CompletedPhases, "all phases must complete in order, including Burn")

	// Burning adds no upload (report-only delivery, SPEC §5).
	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 1, "the report is delivered (report-only delivery, SPEC §5)")

	// SPEC §10.
	report := extractPDFText(t, findUpload(t, uploads, "report.pdf"))
	assert.Contains(t, report, "burned and verified", "report must record the burned discs")

	for _, drive := range drives {
		assert.Containsf(t, report, drive, "report must name burner %s", drive)

		// Independent read-back of a mountable recovery ISO.
		assert.Equalf(t, optical.StateNonBlankRewritable, discState(t, drive),
			"disc on %s must be non-blank after burning", drive)

		files := readOpticalDisc(t, drive)
		assert.NotEmptyf(t, files, "burned disc on %s must contain the recovery files", drive)
	}
}

// TestBackupEndToEnd_OpticalBurnReclaimsNonBlank proves allowNonBlankDiscs lets
// Burn reclaim a non-blank rewritable disc (issue #104 AC2).
func TestBackupEndToEnd_OpticalBurnReclaimsNonBlank(t *testing.T) {
	h := requireHarness(t)
	requireOpticalDiscs(t, h)

	drive := h.opticalDevices[0]

	blankOpticalDisc(t, drive)
	require.NoError(t, optical.NewDisc(drive).WriteImage(t.Context(), makeStaleISO(t)),
		"seed the disc with a stale non-blank image")
	require.Equal(t, optical.StateNonBlankRewritable, discState(t, drive),
		"the disc must be non-blank before the run so the reclaim path is exercised")

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	fixture := prepareBlankTapeAt(t, 5)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-optical-reclaim-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:     1,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery: config.Delivery{
			WebhookURL:  h.deliveryURL(runID),
			OpticalBurn: &config.OpticalBurn{Drives: []string{drive}, Copies: 1, AllowNonBlankDiscs: true},
		},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 15*time.Minute)
	defer cancel()

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, &result),
		"workflow must complete: the non-blank disc is reclaimed, not refused")

	assert.Equal(t, orderedPhases, result.CompletedPhases, "all phases must complete in order, including Burn")

	// The report records the deliberate overwrite (SPEC §10).
	report := extractPDFText(t, findUpload(t, h.rec.uploadsFor(runID), "report.pdf"))
	assert.Contains(t, report, drive, "report must name the reclaimed burner")
	assert.Contains(t, report, "Overwrote a non-blank disc", "report must record the deliberate reclaim")
}

// TestBackupEndToEnd_OpticalBurnRealHardware burns to OPTICAL_BURN_DEV (issue #104
// AC3); skipped when unset.
func TestBackupEndToEnd_OpticalBurnRealHardware(t *testing.T) {
	h := requireHarness(t)
	testutil.SkipIfXorrisoUnavailable(t)

	drive := testutil.OpticalBurnDev(t) // skips when OPTICAL_BURN_DEV is unset

	source := testutil.PoolDataset(t) + "@" + testutil.TestSnapshot(t)
	fixture := prepareBlankTapeAt(t, 6)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-optical-hw-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:     1,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery: config.Delivery{
			WebhookURL: h.deliveryURL(runID),
			// allowNonBlankDiscs re-uses a loaded rewritable disc across runs.
			OpticalBurn: &config.OpticalBurn{Drives: []string{drive}, Copies: 1, AllowNonBlankDiscs: true},
		},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Minute)
	defer cancel()

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, &result),
		"workflow must complete: the disc burns and verifies against the drive")

	assert.Equal(t, orderedPhases, result.CompletedPhases, "all phases must complete in order, including Burn")

	report := extractPDFText(t, findUpload(t, h.rec.uploadsFor(runID), "report.pdf"))
	assert.Contains(t, report, drive, "report must name the real burner")
	assert.Contains(t, report, "burned and verified", "report must record the burned disc")
}

// requireOpticalDiscs skips unless loop burners and xorriso are available.
func requireOpticalDiscs(t *testing.T, h *e2eHarness) {
	t.Helper()

	testutil.SkipIfXorrisoUnavailable(t)

	if len(h.opticalDevices) == 0 {
		t.Skip("optical burn tests require loop-device burners (losetup unavailable in the harness)")
	}
}

// opticalBlankBytes is a prefix comfortably larger than the burned ISO.
const opticalBlankBytes = 128 << 20

// blankOpticalDisc zeros the disc's leading region so it reads back blank.
func blankOpticalDisc(t *testing.T, device string) {
	t.Helper()

	file, err := os.OpenFile(device, os.O_WRONLY, 0)
	require.NoErrorf(t, err, "open %s to blank", device)

	defer func() { _ = file.Close() }()

	zeros := make([]byte, 4<<20)

	for written := 0; written < opticalBlankBytes; {
		chunk := len(zeros)
		if remaining := opticalBlankBytes - written; remaining < chunk {
			chunk = remaining
		}

		n, err := file.Write(zeros[:chunk])
		require.NoErrorf(t, err, "zero %s", device)

		written += n
	}

	require.NoErrorf(t, file.Sync(), "flush blanked %s", device)
}

// discState returns the disc's medium state as the Burn phase reads it.
func discState(t *testing.T, device string) optical.DiscState {
	t.Helper()

	state, err := optical.NewDisc(device).State(t.Context())
	require.NoErrorf(t, err, "read state of %s", device)

	return state
}

// readOpticalDisc mounts the disc read-only and returns its files by relative path.
func readOpticalDisc(t *testing.T, device string) map[string][]byte {
	t.Helper()

	mountpoint := t.TempDir()

	out, err := exec.CommandContext(t.Context(), "mount", "-t", "iso9660", "-o", "ro", device, mountpoint).CombinedOutput()
	require.NoErrorf(t, err, "mount %s: %s", device, strings.TrimSpace(string(out)))

	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		_ = exec.CommandContext(ctx, "umount", mountpoint).Run()
	}()

	files := make(map[string][]byte)

	walkErr := filepath.WalkDir(mountpoint, func(pathName string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(mountpoint, pathName)
		if err != nil {
			return err
		}

		data, err := os.ReadFile(pathName)
		if err != nil {
			return err
		}

		files[filepath.ToSlash(rel)] = data

		return nil
	})
	require.NoError(t, walkErr, "read back disc contents")

	return files
}

// makeStaleISO builds a small marker ISO standing in for a used disc's contents.
func makeStaleISO(t *testing.T) string {
	t.Helper()

	stage := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stage, "stale-marker.txt"),
		[]byte("stale contents from a prior run, to be reclaimed\n"), 0o644))

	isoPath := filepath.Join(t.TempDir(), "stale.iso")

	out, err := exec.CommandContext(t.Context(), "xorriso",
		"-as", "mkisofs", "-o", isoPath, "-V", "STALE", stage).CombinedOutput()
	require.NoErrorf(t, err, "build stale ISO: %s", strings.TrimSpace(string(out)))

	return isoPath
}
