//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// TestRecoveryFromReportAlone is the report's real acceptance test (issue #340):
// it recovers a written tape using nothing but what the delivered report says.
//
// Every fact it acts on — the mount command, the on-tape layout, the per-file
// checksums, whether the archive is compressed, and the decryption identity — is
// read out of the rendered PDF, never from the run config or this repository. A
// recoverer twenty years from now holds that printout and the tapes; if a step
// cannot be followed from the page, the report has failed at its job however
// correct the bytes on the tape are.
//
// It deliberately does not exercise the disc-manifest verification of printed step
// 4: that artifact ships on the recovery ISO, which a run without optical burning
// never produces. The per-file checksums printed in the report cover the same
// ground here.
func TestRecoveryFromReportAlone(t *testing.T) {
	h := requireHarness(t)

	snapshot := testutil.TestSnapshot(t)
	if snapshot == "" {
		t.Skipf("%s is not set; no known snapshot to recover and compare against", testutil.EnvTestSnapshot)
	}

	source := testutil.PoolDataset(t) + "@" + snapshot
	// Slot 5 is this test's own tape, distinct from the FullRun (2),
	// verify-fault (2), k8s-source (3), and MultipleDriveSets (8, 9) tests.
	fixture := prepareBlankTapeAt(t, 5)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-recovery-%d", time.Now().UnixNano())

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

	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 1, "the report is delivered")

	report := extractPDFText(t, findUpload(t, uploads, "report.pdf"))

	// From here on, only `report` may be consulted.
	mountCmd := reportMountCommand(t, report)
	escrowed := reportIdentity(t, report)
	checksums := reportChecksums(t, report)
	directory := reportArchiveDirectory(t, report)
	compressed := reportSaysCompressed(t, report)

	require.NotEmpty(t, checksums, "the report must print a checksum for every on-tape file")

	work := t.TempDir()
	mountpoint := filepath.Join(work, "tape")
	require.NoError(t, os.Mkdir(mountpoint, 0o755))

	// Printed step 1-2: mount the volume read-only with the command as printed,
	// substituting only the placeholders the text itself names.
	sgDevice := testutil.Drive0SgDev(t)
	loadTapeForRecovery(t, fixture)

	mount := strings.NewReplacer("/dev/sgN", sgDevice, "<mountpoint>", mountpoint).Replace(mountCmd)
	runRecoveryStep(t, work, "sh", "-c", mount)

	t.Cleanup(func() {
		// The mount is a FUSE mount, so it is released with fusermount, which
		// needs no privileges — the same way pkg/ltfs detaches its own mounts.
		_ = exec.Command("fusermount", "-u", mountpoint).Run()
	})

	// Printed step 3: copy the archives tree into a barcode-named directory.
	barcode := string(fixture.barcode)
	require.NoError(t, os.Mkdir(filepath.Join(work, barcode), 0o755))
	runRecoveryStep(t, work, "cp", "-r", filepath.Join(mountpoint, "archives"), filepath.Join(work, barcode))

	// Printed step 4: verify every copied file against the checksums the report
	// prints, so corruption is caught before anything is decrypted.
	archiveDir := filepath.Join(work, barcode, directory)

	for name, want := range checksums {
		got := sha256Of(t, filepath.Join(archiveDir, name))
		assert.Equalf(t, want, got, "%s must match the checksum printed in the report", name)
	}

	// Printed step 5: concatenate the slices, in order, into the age stream.
	slices, err := filepath.Glob(filepath.Join(archiveDir, "archive.[0-9][0-9][0-9]"))
	require.NoError(t, err)
	require.NotEmpty(t, slices, "the archive must have at least one slice")
	sort.Strings(slices)

	stream := filepath.Join(work, "archive.age")
	concatenate(t, stream, slices)

	// Printed step 6: decrypt with the identity the report escrows.
	identityFile := filepath.Join(work, "identity.txt")
	require.NoError(t, os.WriteFile(identityFile, []byte(escrowed+"\n"), 0o600))

	decrypted := filepath.Join(work, "archive.tar.zst")
	runRecoveryStep(t, work, "age", "-d", "-i", identityFile, "-o", decrypted, stream)

	// Printed step 7: decompress only when the report says the archive is
	// compressed — the recoverer follows the report, not the bytes.
	tarball := filepath.Join(work, "archive.tar")
	if compressed {
		runRecoveryStep(t, work, "zstd", "-d", decrypted, "-o", tarball)
	} else {
		require.NoError(t, os.Rename(decrypted, tarball))
	}

	// Printed step 8: unpack.
	unpacked := filepath.Join(work, "unpacked")
	require.NoError(t, os.Mkdir(unpacked, 0o755))
	runRecoveryStep(t, work, "tar", "-xf", tarball, "-C", unpacked)

	// The recovered tree must be the source snapshot, file for file. This is the
	// only assertion that reaches outside the report: it is what "recovered"
	// means.
	sourceTree := filepath.Join(testutil.PoolMount(t), ".zfs", "snapshot", snapshot)
	assertTreesIdentical(t, sourceTree, unpacked)
}

// reportMountCommand extracts the LTFS mount command the report prints. A command
// that names no device cannot be followed at all, so the regexp requires the
// device option to be part of what was printed.
func reportMountCommand(t *testing.T, report string) string {
	t.Helper()

	pattern := regexp.MustCompile(`ltfs\s+-o\s+ro\s+-o\s+devname=\S+\s+<mountpoint>`)

	match := pattern.FindString(collapseSpaces(report))
	require.NotEmpty(t, match,
		"the report must print a mount command naming the device to open; got report:\n%s", report)

	return match
}

// reportIdentity extracts the escrowed age private identity from the report.
func reportIdentity(t *testing.T, report string) string {
	t.Helper()

	match := regexp.MustCompile(`AGE-SECRET-KEY-PQ-1[A-Z0-9]+`).FindString(collapseSpaces(report))
	require.NotEmpty(t, match, "the report must escrow the age private identity")

	return match
}

// reportChecksums pulls the printed contents manifest's file name / SHA-256 pairs
// out of the report text: the rows a recoverer verifies their copy against.
func reportChecksums(t *testing.T, report string) map[string]string {
	t.Helper()

	pattern := regexp.MustCompile(`(archive\.(?:\d{3}|par2|vol\d+\+\d+\.par2))\s+[\d,]+\s+([0-9a-f]{64})`)

	checksums := make(map[string]string)
	for _, match := range pattern.FindAllStringSubmatch(collapseSpaces(report), -1) {
		checksums[match[1]] = match[2]
	}

	return checksums
}

// reportArchiveDirectory extracts the archive's on-tape directory, so the copy is
// laid out where the report says its files live.
func reportArchiveDirectory(t *testing.T, report string) string {
	t.Helper()

	match := regexp.MustCompile(`archives/\d{3}-[a-z0-9._-]+`).FindString(collapseSpaces(report))
	require.NotEmpty(t, match, "the report must name the archive's on-tape directory")

	return match
}

// reportSaysCompressed reads the archive's compression from the report, which is
// what tells a recoverer whether step 7's zstd pass applies.
func reportSaysCompressed(t *testing.T, report string) bool {
	t.Helper()

	collapsed := collapseSpaces(report)

	zstd := strings.Contains(collapsed, "Compression zstd")
	none := strings.Contains(collapsed, "Compression none")
	require.Truef(t, zstd != none,
		"the report must state each archive's compression exactly once, so the recoverer knows whether to run zstd")

	return zstd
}

// collapseSpaces folds every run of whitespace to a single space, so patterns
// match regardless of how the PDF extractor wrapped or spaced the text.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// loadTapeForRecovery puts the run's tape back in the drive, standing in for the
// operator of printed step 1 carrying a cartridge to a standalone drive. The run
// left it in its storage slot.
func loadTapeForRecovery(t *testing.T, fixture tapeFixture) {
	t.Helper()

	require.NoError(t, fixture.changer.Load(t.Context(), fixture.slotAddr, fixture.driveAddr),
		"load the written tape for recovery")

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 60*time.Second)
		defer cancel()

		_ = fixture.changer.Unload(ctx, fixture.slotAddr, fixture.driveAddr)
	})

	testutil.SkipIfDriveNotReady(t, testutil.Drive0Dev(t))
}

// runRecoveryStep runs one step of the printed procedure, failing the test with
// the tool's own output when the step does not work as printed.
func runRecoveryStep(t *testing.T, dir string, name string, args ...string) {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "recovery step %q failed:\n%s", append([]string{name}, args...), out)
}

// concatenate writes parts, in order, to path — printed step 5's cat.
func concatenate(t *testing.T, path string, parts []string) {
	t.Helper()

	out, err := os.Create(path)
	require.NoError(t, err)

	defer func() { require.NoError(t, out.Close()) }()

	for _, part := range parts {
		data, err := os.ReadFile(part)
		require.NoError(t, err)

		_, err = out.Write(data)
		require.NoError(t, err)
	}
}

// sha256Of returns a file's lowercase hex SHA-256, using the system sha256sum the
// report's verification step names.
func sha256Of(t *testing.T, path string) string {
	t.Helper()

	out, err := exec.Command("sha256sum", path).Output()
	require.NoErrorf(t, err, "sha256sum %s", path)

	return strings.Fields(string(out))[0]
}

// assertTreesIdentical asserts want and got hold the same files with the same
// contents, using diff so a mismatch reports which file differs.
func assertTreesIdentical(t *testing.T, want, got string) {
	t.Helper()

	out, err := exec.Command("diff", "-rq", want, got).CombinedOutput()
	assert.NoErrorf(t, err, "the recovered tree must match the source snapshot:\n%s", out)
}
