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

// This file is the final layer of parent #98: it proves the optical Burn phase
// (SPEC §10) is wired end-to-end into the workflow and data worker, the way the
// tape path is proven against mhvtl. There is no faithful virtual optical writer,
// so the burners are loop-device-backed pseudo-discs (setupOpticalDiscs) driven
// through xorriso's stdio pseudo-drive — the same mechanism pkg/optical and the
// burn integration tests use — plus an opt-in real-hardware burn gated on
// OPTICAL_BURN_DEV.

// TestBackupEndToEnd_OpticalBurn drives a full backup with optical burning enabled
// against loop-device burners, proving the Burn phase is wired into the workflow and
// data worker (issue #104 AC1). With two burners and two copies the discs burn in a
// single set — no operator disc-swap pause — and each is read back and verified
// against the recovery-disc manifest by the in-workflow VerifyDisc activity
// (MaximumAttempts=1), so a completed run means every copy burned and verified.
func TestBackupEndToEnd_OpticalBurn(t *testing.T) {
	h := requireHarness(t)
	requireOpticalDiscs(t, h)

	// Two burners, two copies: exactly one burn-set, so no manual disc-swap pause.
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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
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

	// Every phase ran to completion, in order — including Burn between Report and
	// Deliver.
	assert.Equal(t, orderedPhases, result.CompletedPhases, "all phases must complete in order, including Burn")

	// The run still delivers exactly the report (report-only delivery, SPEC §5);
	// burning re-renders the report but adds no upload — the ISO's durable home is
	// the burned disc, not a Discord upload.
	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 1, "the report is delivered (report-only delivery, SPEC §5)")

	// The re-rendered report records every burned disc, naming its burner and that
	// it was burned and verified (SPEC §10).
	report := extractPDFText(t, findUpload(t, uploads, "report.pdf"))
	assert.Contains(t, report, "burned and verified", "report must record the burned discs")

	for _, drive := range drives {
		assert.Containsf(t, report, drive, "report must name burner %s", drive)

		// Independent read-back: each disc really holds a mountable recovery ISO
		// (the workflow's VerifyDisc already checked it against the manifest).
		assert.Equalf(t, optical.StateNonBlankRewritable, discState(t, drive),
			"disc on %s must be non-blank after burning", drive)

		files := readOpticalDisc(t, drive)
		assert.NotEmptyf(t, files, "burned disc on %s must contain the recovery files", drive)
	}
}

// TestBackupEndToEnd_OpticalBurnReclaimsNonBlank proves that with allowNonBlankDiscs
// set the Burn phase reclaims and re-uses a non-blank rewritable disc instead of
// requiring a fresh blank (issue #104 AC2) — the loop-device analogue of the
// real-hardware disc-reuse ergonomics. A loop disc is pre-populated with a stale
// image so it is genuinely non-blank; without the opt-in the run would pause and
// refuse it, so a completed run that records the overwrite proves the reclaim.
func TestBackupEndToEnd_OpticalBurnReclaimsNonBlank(t *testing.T) {
	h := requireHarness(t)
	requireOpticalDiscs(t, h)

	drive := h.opticalDevices[0]

	// Pre-populate the disc with an unrelated image so it is non-blank going in.
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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
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

	// The re-rendered report records the deliberate overwrite of the non-blank disc
	// (SPEC §10), and the completed run means the reclaimed disc verified against the
	// recovery manifest — it now holds the recovery image, not the stale one.
	report := extractPDFText(t, findUpload(t, h.rec.uploadsFor(runID), "report.pdf"))
	assert.Contains(t, report, drive, "report must name the reclaimed burner")
	assert.Contains(t, report, "Overwrote a non-blank disc", "report must record the deliberate reclaim")
}

// TestBackupEndToEnd_OpticalBurnRealHardware burns to a real optical drive named by
// OPTICAL_BURN_DEV — the only way to exercise a physical MMC write (issue #104 AC3).
// It is skipped when the env var is absent, so make test-e2e stays green in CI
// without optical hardware; it is gated exactly like the real-tape and benchmark
// tests. Load a rewritable disc and it re-uses it across runs (allowNonBlankDiscs).
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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery: config.Delivery{
			WebhookURL: h.deliveryURL(runID),
			// One drive, one copy: a single burn-set, no operator disc-swap pause.
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

// requireOpticalDiscs skips the calling test unless the harness provisioned
// loop-device burners (losetup available) and the host has xorriso, which the test
// itself uses to read disc state and pre-burn stale images.
func requireOpticalDiscs(t *testing.T, h *e2eHarness) {
	t.Helper()

	testutil.SkipIfXorrisoUnavailable(t)

	if len(h.opticalDevices) == 0 {
		t.Skip("optical burn tests require loop-device burners (losetup unavailable in the harness)")
	}
}

// opticalBlankBytes is how much of a loop-device pseudo-disc blankOpticalDisc zeros
// to reset it between tests. The burned recovery ISO (tens of MB) is written from
// the start of the device, so zeroing a generous prefix wipes it entirely and the
// device reads back blank; the untouched tail was already zero from setup.
const opticalBlankBytes = 128 << 20

// blankOpticalDisc resets a loop-device pseudo-disc to blank by zeroing its leading
// region, so a test starts from a known-blank disc regardless of what an earlier
// test burned there. The e2e suite runs as root, so it can write the device node
// directly.
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

// discState returns the medium state of the disc loaded in device, failing the test
// on an operational error. It reads the same xorriso-backed state the Burn phase
// uses.
func discState(t *testing.T, device string) optical.DiscState {
	t.Helper()

	state, err := optical.NewDisc(device).State(t.Context())
	require.NoErrorf(t, err, "read state of %s", device)

	return state
}

// readOpticalDisc mounts a burned disc read-only and returns its regular files by
// disc-relative path, an independent read-back of what the Burn phase wrote. It
// mounts the block device directly (an ISO 9660 filesystem needs root, which the
// suite has) and unmounts before returning.
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

// makeStaleISO builds a small ISO 9660 image with a uniquely named marker file,
// standing in for a used disc's prior contents so the reclaim test starts from a
// genuinely non-blank disc. It returns the image path.
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
