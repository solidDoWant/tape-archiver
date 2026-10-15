//go:build integration

package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ledongthuc/pdf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// TestRecoveryFromReportAlone is the report's real acceptance test (issue #340):
// it runs a backup, then recovers the written tape using nothing but what the
// delivered report says.
//
// Every fact it acts on — the mount command, the on-tape layout, the per-file
// checksums, whether the archive is compressed, and the decryption identity — is
// read out of the rendered PDF, never from the run config or this repository. A
// recoverer decades from now holds that printout and the tapes; if a step cannot be
// followed from the page, the report has failed at its job however correct the bytes
// on the tape are. Each of the report defects this issue fixes would fail a step
// here.
//
// It deliberately does not exercise the disc-manifest verification of printed step
// 4: that artifact ships on the recovery ISO, which a run without optical burning
// never produces. The per-file checksums printed in the report cover the same
// ground.
func TestRecoveryFromReportAlone(t *testing.T) {
	requireTemporalAddress(t)
	testutil.SkipIfMhvtlUnavailable(t)
	testutil.SkipIfLTFSUnavailable(t)
	testutil.SkipIfZFSUnavailable(t)
	testutil.SkipIfPoolUnavailable(t)
	requireBinaries(t, "age", "age-keygen", "par2", "zstd", "tar", "sha256sum", "ltfs")

	snapshot := testutil.TestSnapshot(t)
	if snapshot == "" {
		t.Skipf("%s not set; run via `make test-integration`", testutil.EnvTestSnapshot)
	}

	source := testutil.PoolDataset(t) + "@" + snapshot

	changer := tape.NewChanger(testutil.ChangerDev(t))

	inv, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")
	require.GreaterOrEqual(t, len(inv.Drives), 1, "at least one drive required")
	require.False(t, inv.Drives[0].Loaded, "drive 0 must start empty")

	// Storage slot index 8 is this test's alone on the shared mhvtl library
	// (indexes 0-7 belong to the session, tape-path, whole-run, medium-generation
	// and multi-drive-set tests).
	require.GreaterOrEqual(t, len(inv.Slots), 9, "at least nine storage slots required")
	slot := inv.Slots[8]
	require.True(t, slot.Full, "slot index 8 must hold a tape")
	require.NotEmpty(t, slot.Barcode, "slot index 8 tape must have a barcode")

	stDev := testutil.Drive0Dev(t)
	sgDev := testutil.Drive0SgDev(t)
	driveAddr := inv.Drives[0].Address
	slotAddr := slot.Address
	barcode := slot.Barcode

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 240*time.Second)
		defer cancel()

		returnAndBlank(ctx, changer, stDev, sgDev, driveAddr, slotAddr, barcode)
	})

	require.NoError(t, changer.Load(t.Context(), slotAddr, driveAddr), "pre-load for readiness/blanking")
	testutil.SkipIfDriveNotReady(t, stDev)
	eraseLoadedTape(t.Context(), stDev, sgDev)
	require.NoError(t, changer.Unload(t.Context(), slotAddr, driveAddr), "unload after blanking")

	report := runBackupCapturingReport(t, source, stDev, slotAddr)

	// From here on, only the rendered report may be consulted.
	mountCmd := reportMountCommand(t, report)
	escrowed := reportIdentity(t, report)
	checksums := reportChecksums(t, report)
	directory := reportArchiveDirectory(t, report)
	compressed := reportSaysCompressed(t, report)

	require.NotEmpty(t, checksums, "the report must print a checksum for every on-tape file")

	work := t.TempDir()
	mountpoint := filepath.Join(work, "tape")
	require.NoError(t, os.Mkdir(mountpoint, 0o755))

	// Printed steps 1-2: load a tape and mount its volume read-only with the
	// command exactly as printed, substituting only the placeholders the text
	// itself names. The run's Eject phase parked the tape in the I/O station, so
	// it is fetched by barcode — the identity a recoverer reads off the cartridge.
	loadWrittenTape(t, changer, inv, barcode, driveAddr)

	mount := strings.NewReplacer("/dev/sgN", sgDev, "<mountpoint>", mountpoint).Replace(mountCmd)
	mountTape(t, work, mount, mountpoint)

	t.Cleanup(func() { _ = exec.Command("fusermount", "-u", mountpoint).Run() })

	// Printed step 3: copy the archives tree into a barcode-named directory.
	require.NoError(t, os.Mkdir(filepath.Join(work, string(barcode)), 0o755))
	runRecoveryStep(t, work, "cp", "-r", filepath.Join(mountpoint, "archives"), filepath.Join(work, string(barcode)))

	// Printed step 4: verify every copied file against the checksums the report
	// prints, so corruption is caught before anything is decrypted.
	archiveDir := filepath.Join(work, string(barcode), directory)

	for name, want := range checksums {
		assert.Equalf(t, want, sha256Of(t, filepath.Join(archiveDir, name)),
			"%s must match the checksum printed in the report", name)
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

	// Printed step 7: decompress only when the report says this archive is
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
	// only assertion that reaches outside the report: it is what "recovered" means.
	// It also holds the printed unpack step to account — a single-volume archive
	// lands with no wrapping directory, exactly as step 8 says.
	sourceTree := filepath.Join(testutil.PoolMount(t), ".zfs", "snapshot", snapshot)

	out, err := exec.Command("diff", "-rq", sourceTree, unpacked).CombinedOutput()
	assert.NoErrorf(t, err, "the recovered tree must match the source snapshot:\n%s", out)
}

// runBackupCapturingReport runs one backup of source onto the tape in slotAddr
// with real control and data workers, and returns the extracted text of the PDF
// report it delivers.
func runBackupCapturingReport(t *testing.T, source, stDev string, slotAddr int) string {
	t.Helper()

	temporalClient := dialTemporal(t)

	controlWorker := worker.New(temporalClient, TaskQueue, worker.Options{})
	RegisterControl(controlWorker, ControlConfig{})
	require.NoError(t, controlWorker.Start(), "start control worker")
	t.Cleanup(controlWorker.Stop)

	dataWorker := worker.New(temporalClient, DataTaskQueue, worker.Options{EnableSessionWorker: true})
	RegisterData(dataWorker, DataConfig{
		StagingDir:          t.TempDir(),
		RecoveryBinariesDir: testutil.RecoveryBinariesDir(t),
		RecoverySourcesDir:  testutil.RecoverySourcesDir(t),
	})
	require.NoError(t, dataWorker.Start(), "start data worker")
	t.Cleanup(dataWorker.Stop)

	var (
		mu        sync.Mutex
		delivered []byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if file, _, formErr := r.FormFile("files[0]"); formErr == nil {
			data, _ := io.ReadAll(file)

			mu.Lock()
			delivered = data
			mu.Unlock()
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	identity, recipient := generateTestKeypair(t)

	cfg := config.Config{
		Sources: []config.Source{{ZFSPath: &config.ZFSPathSource{Name: source}}},
		Copies:  1,
		Library: config.Library{
			Changer:           testutil.ChangerDev(t),
			Drives:            []string{stDev},
			BlankSlots:        []int{slotAddr},
			TapeCapacityBytes: 2_500_000_000_000,
		},
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: server.URL},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 6*time.Minute)
	defer cancel()

	run, err := temporalClient.ExecuteWorkflow(runCtx, client.StartWorkflowOptions{
		ID:        fmt.Sprintf("recovery-backup-%d", time.Now().UnixNano()),
		TaskQueue: TaskQueue,
	}, WorkflowType, cfg)
	require.NoError(t, err, "start workflow")

	var result Result
	require.NoError(t, run.Get(runCtx, &result), "workflow must complete successfully")

	mu.Lock()
	defer mu.Unlock()

	require.NotEmpty(t, delivered, "the run must deliver a report to recover from")

	return extractReportText(t, delivered)
}

// extractReportText parses the delivered PDF and returns its plain text.
func extractReportText(t *testing.T, reportPDF []byte) string {
	t.Helper()

	reader, err := pdf.NewReader(bytes.NewReader(reportPDF), int64(len(reportPDF)))
	require.NoError(t, err, "the delivered report must be a valid PDF")

	textReader, err := reader.GetPlainText()
	require.NoError(t, err)

	text, err := io.ReadAll(textReader)
	require.NoError(t, err)

	return string(text)
}

// reportMountCommand extracts the LTFS mount command the report prints. A command
// naming no device cannot be followed at all, so the pattern requires the device
// option to be part of what was printed.
func reportMountCommand(t *testing.T, report string) string {
	t.Helper()

	match := regexp.MustCompile(`ltfs\s+-o\s+ro\s+-o\s+devname=\S+\s+<mountpoint>`).FindString(collapseSpaces(report))
	require.NotEmpty(t, match, "the report must print a mount command naming the device to open")

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
// out of the report: the rows a recoverer verifies their copy against.
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

	match := regexp.MustCompile(`archives/\d{3}-[a-zA-Z0-9._-]+`).FindString(collapseSpaces(report))
	require.NotEmpty(t, match, "the report must name the archive's on-tape directory")

	return match
}

// reportSaysCompressed reads the archive's compression from the report, which is
// what tells a recoverer whether the printed decompress step applies.
func reportSaysCompressed(t *testing.T, report string) bool {
	t.Helper()

	collapsed := collapseSpaces(report)

	zstd := strings.Contains(collapsed, "Compression zstd")
	none := strings.Contains(collapsed, "Compression none")
	require.True(t, zstd != none,
		"the report must state the archive's compression exactly once, so a recoverer knows whether to run zstd")

	return zstd
}

// collapseSpaces folds every run of whitespace to a single space, so patterns
// match regardless of how the PDF extractor wrapped or spaced the text.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// loadWrittenTape puts the run's tape back in the drive, standing in for the
// operator of printed step 1 carrying a cartridge to a standalone drive. It
// locates the tape by barcode: the run's Eject phase parks written tapes in the
// I/O station, so it is no longer in the slot it was loaded from.
func loadWrittenTape(t *testing.T, changer *tape.Changer, inv tape.Inventory, barcode tape.Barcode, driveAddr int) {
	t.Helper()

	current, err := changer.Inventory(t.Context())
	require.NoError(t, err, "inventory")

	source := -1

	for _, slot := range current.Slots {
		if slot.Full && slot.Barcode == barcode {
			source = slot.Address
		}
	}

	for _, slot := range current.IOSlots {
		if slot.Full && slot.Barcode == barcode {
			// Load only takes storage slots, so bring it back in first.
			for _, storage := range current.Slots {
				if !storage.Full {
					require.NoError(t, changer.Transfer(t.Context(), slot.Address, storage.Address), "move the written tape out of the I/O station")

					source = storage.Address

					break
				}
			}
		}
	}

	require.NotEqualf(t, -1, source, "the written tape %s must be somewhere in the library", barcode)

	require.NoError(t, changer.Load(t.Context(), source, driveAddr), "load the written tape for recovery")

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 60*time.Second)
		defer cancel()

		_ = changer.Unload(ctx, source, driveAddr)
	})
}

// mountTape runs the report's printed mount command, retrying while the drive
// settles after the load. A drive that has just taken a cartridge — moments after
// the run's own write and eject — can still report not-ready, which is a timing
// property of the library rather than anything the report got wrong.
//
// It fails, and deliberately never skips, once the budget is spent: a skip here
// would report the suite green while the very thing this test exists to check —
// that the printed procedure works — went unexercised.
func mountTape(t *testing.T, dir, command, mountpoint string) {
	t.Helper()

	const (
		attempts = 12
		delay    = 5 * time.Second
	)

	var out []byte

	for attempt := range attempts {
		cmd := exec.Command("sh", "-c", command)
		cmd.Dir = dir

		var err error
		if out, err = cmd.CombinedOutput(); err == nil {
			// ltfs daemonizes on success; the volume is mounted once its root lists.
			if entries, readErr := os.ReadDir(mountpoint); readErr == nil && len(entries) > 0 {
				return
			}
		}

		if attempt < attempts-1 {
			time.Sleep(delay)
		}
	}

	require.Failf(t, "the report's mount command never mounted the tape",
		"after %d attempts over %s, running %q left %s empty. Last output:\n%s",
		attempts, time.Duration(attempts)*delay, command, mountpoint, out)
}

// runRecoveryStep runs one step of the printed procedure, failing the test with
// the tool's own output when the step does not work as printed.
func runRecoveryStep(t *testing.T, dir, name string, args ...string) {
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
