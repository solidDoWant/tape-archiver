package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/optical"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// reportTestInput builds a small but complete ReportInput: one raw-ZFS archive
// with two slices and one PAR2 file, packed onto a single logical tape, written
// as two copies (two physical tapes / barcodes).
func reportTestInput(t *testing.T) ReportInput {
	t.Helper()

	staged := StagedArchive{
		SourceIndex: 0,
		Slices: []StagedSlice{
			{Path: "/stage/000/archive.000", SHA256: "aa", SizeBytes: 100},
			{Path: "/stage/000/archive.001", SHA256: "bb", SizeBytes: 50},
		},
		SizeBytes: 150,
	}

	par2 := PAR2Set{
		SourceIndex:       0,
		RedundancyPercent: 10,
		Files:             []StagedSlice{{Path: "/stage/000/archive.par2", SHA256: "cc", SizeBytes: 20}},
	}

	return ReportInput{
		Config: config.Config{
			Sources:    []config.Source{{ZFSPath: &config.ZFSPathSource{Name: "bulk-pool-01/archive@snap"}}},
			Copies:     2,
			Library:    config.Library{TapeCapacityBytes: 2_500_000_000_000},
			Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
			Encryption: config.Encryption{Recipients: []string{"age1pq1recipient"}, Identity: "AGE-SECRET-KEY-PQ-1TEST"},
		},
		RunID:          "run-123",
		Date:           time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		SliceSizeBytes: 1 << 30,
		Resolved:       []ResolvedArchive{{SourceIndex: 0, Label: "archive", Snapshots: []ResolvedSnapshot{{ZFSPath: "bulk-pool-01/archive@snap"}}}},
		Staged:         []StagedArchive{staged},
		PAR2:           []PAR2Set{par2},
		Plan: TapePlan{
			Copies: 2,
			Tapes:  []PlannedTape{{Archives: []PlannedArchive{{SourceIndex: 0, DataBytes: 150, PAR2ReservedBytes: 20}}}},
		},
		Written: []WrittenTape{
			{Barcode: tape.Barcode("TAPE01L6"), TapeIndex: 0, CopyIndex: 0, IndexXMLPath: stageTestIndex(t, "TAPE01L6")},
			{Barcode: tape.Barcode("TAPE02L6"), TapeIndex: 0, CopyIndex: 1, IndexXMLPath: stageTestIndex(t, "TAPE02L6")},
		},
	}
}

// stageTestIndex writes a captured LTFS index for barcode to a temp file and
// returns its path, mirroring how FinalizeTape stages the index to disk for the
// Report phase to read back (issue #221).
func stageTestIndex(t *testing.T, barcode string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), barcode+".xml")
	require.NoError(t, os.WriteFile(path, []byte("<ltfsindex/>"), 0o644))

	return path
}

func ptrFloat(f float64) *float64 { return &f }

// TestBuildReportManifest checks the pure manifest assembly maps the run state
// into every SPEC §9 field: contents, tapes-by-barcode, build metadata, and the
// escrowed identity.
func TestBuildReportManifest(t *testing.T) {
	t.Parallel()

	input := reportTestInput(t)
	device := deviceIdentity{
		driveModel:      "IBM ULT3580-HH8",
		driveSerial:     "SN123",
		driveGeneration: "LTO-6",
		libraryModel:    "IBM 3573-TL",
	}

	manifest := buildReportManifest(input, device)

	assert.Equal(t, "run-123", manifest.RunID)
	assert.Equal(t, input.Date, manifest.Date)
	assert.Equal(t, "AGE-SECRET-KEY-PQ-1TEST", manifest.AgeIdentity)
	assert.NotEmpty(t, manifest.RecoveryProcedure)

	require.Len(t, manifest.Archives, 1)
	archive := manifest.Archives[0]
	assert.Equal(t, "bulk-pool-01/archive@snap", archive.Name)
	assert.Equal(t, "archives/000-archive", archive.Directory,
		"the report must show each archive's descriptive on-tape directory")
	assert.Equal(t, []string{"bulk-pool-01/archive@snap"}, archive.SourceSnapshots)
	// slices then PAR2 file, by base name.
	require.Len(t, archive.Files, 3)
	assert.Equal(t, "archive.000", archive.Files[0].Name)
	assert.Equal(t, "archive.par2", archive.Files[2].Name)
	assert.Equal(t, "cc", archive.Files[2].SHA256)

	// Both physical tapes are listed by barcode, each holding the one archive.
	require.Len(t, manifest.Tapes, 2)
	assert.Equal(t, "TAPE01L6", manifest.Tapes[0].Barcode)
	assert.Equal(t, []string{"bulk-pool-01/archive@snap"}, manifest.Tapes[0].Contents)
	assert.Equal(t, "TAPE02L6", manifest.Tapes[1].Barcode)

	assert.Equal(t, "IBM ULT3580-HH8", manifest.Build.DriveModel)
	assert.Equal(t, "SN123", manifest.Build.DriveSerial)
	assert.Equal(t, "LTO-6", manifest.Build.DriveGeneration)
	assert.Equal(t, "10%", manifest.Build.PAR2Redundancy)
	assert.Equal(t, int64(1<<30), manifest.Build.SliceSize)
	assert.NotEmpty(t, manifest.Build.ToolVersion)
	assert.NotEmpty(t, manifest.Build.AgeVersion)
}

// TestReportTapesPropagatesOverwroteNonBlank checks that a WrittenTape marked as
// having overwritten a non-blank tape (Library.AllowNonBlankTapes) surfaces the
// flag on its report.Tape, while a normally-written tape does not (SPEC §9).
func TestReportTapesPropagatesOverwroteNonBlank(t *testing.T) {
	t.Parallel()

	input := reportTestInput(t)
	input.Written[0].OverwroteNonBlank = true

	manifest := buildReportManifest(input, deviceIdentity{})

	require.Len(t, manifest.Tapes, 2)
	assert.True(t, manifest.Tapes[0].OverwroteNonBlank,
		"an overwritten non-blank tape must be flagged in the report")
	assert.False(t, manifest.Tapes[1].OverwroteNonBlank,
		"a tape written to a blank tape must not be flagged as an overwrite")
}

// TestReportDiscsPropagatesOverwrite checks the burned discs map into the report
// manifest, carrying each burner device and any deliberate overwrite (SPEC §10),
// and that a run without burning yields no Discs section.
func TestReportDiscsPropagatesOverwrite(t *testing.T) {
	t.Parallel()

	input := reportTestInput(t)
	input.Discs = []BurnResult{
		{Device: "/dev/sr0"},
		{Device: "/dev/sr1", OverwroteNonBlank: true},
	}

	manifest := buildReportManifest(input, deviceIdentity{})

	require.Len(t, manifest.Discs, 2)
	assert.Equal(t, "/dev/sr0", manifest.Discs[0].Device)
	assert.False(t, manifest.Discs[0].OverwroteNonBlank, "a disc burned to a blank medium is not an overwrite")
	assert.True(t, manifest.Discs[1].OverwroteNonBlank, "a reclaimed non-blank disc must be flagged in the report")

	input.Discs = nil
	assert.Empty(t, buildReportManifest(input, deviceIdentity{}).Discs,
		"a run without optical burning renders no Discs section")
}

// TestBuildReportStagesDiscManifest checks that with optical burning enabled the
// Report phase stages the disc-content manifest beside the uncompressed ISO and
// records its path (SPEC §10). The manifest lists the ISO's own files with SHA-256
// digests, so the Burn phase's VerifyDisc can read each burned disc back against
// it — distinct from the on-tape SHA-256 manifest embedded inside the ISO.
func TestBuildReportStagesDiscManifest(t *testing.T) {
	t.Parallel()

	identity, recipient := generateTestKeypair(t)

	input := reportTestInput(t)
	input.Config.Encryption = config.Encryption{Recipients: []string{recipient}, Identity: identity}
	input.Config.Delivery.OpticalBurn = &config.OpticalBurn{Drives: []string{"/dev/sr0"}, Copies: 1}

	acts := newReportActivities(t.TempDir(), testutil.RecoveryBinariesDir(t), testutil.RecoverySourcesDir(t))

	outDir := t.TempDir()

	output, err := acts.buildReport(t.Context(), outDir, input)
	require.NoError(t, err)

	require.Equal(t, filepath.Join(outDir, discManifestFileName), output.DiscManifestPath)

	data, err := os.ReadFile(output.DiscManifestPath)
	require.NoError(t, err)

	manifest, err := optical.ParseManifest(bytes.NewReader(data))
	require.NoError(t, err)

	// The disc-content manifest names the recovery ISO's own files, not the on-tape
	// files listed inside it.
	for _, discPath := range []string{"report.pdf", "manifest.sha256", "recovery-procedure.md"} {
		assert.Contains(t, manifest, discPath, "disc manifest must list the ISO's own %s", discPath)
	}

	assert.Contains(t, manifest, "ltfs-index/tape01l6.schema",
		"disc manifest must list each tape's LTFS index backup at its lowercased read-back path")

	for discPath, digest := range manifest {
		assert.Lenf(t, digest, 64, "digest for %s must be a hex SHA-256", discPath)
	}
}

// TestRebuildDeliveredReport checks the post-burn re-render (SPEC §10): it
// overwrites the delivered report.pdf at the same path with a fresh render that
// records the burned discs, leaving any other staged artifact untouched.
func TestRebuildDeliveredReport(t *testing.T) {
	t.Parallel()

	identity, recipient := generateTestKeypair(t)

	input := reportTestInput(t)
	input.Config.Encryption = config.Encryption{Recipients: []string{recipient}, Identity: identity}

	acts := newReportActivities(t.TempDir(), testutil.RecoveryBinariesDir(t), testutil.RecoverySourcesDir(t))
	outDir := t.TempDir()

	// Pre-burn build: the delivered report predates the burn, so it records no discs.
	preBurn, err := acts.rebuildReport(t.Context(), outDir, input)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(outDir, reportFileName), preBurn)

	before, err := os.ReadFile(preBurn)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(before), "%PDF-"), "delivered report must be a PDF")

	// Post-burn re-render with the burned discs (one deliberately reclaimed).
	input.Discs = []BurnResult{{Device: "/dev/sr0", OverwroteNonBlank: true}}

	rebuilt, err := acts.rebuildReport(t.Context(), outDir, input)
	require.NoError(t, err)
	assert.Equal(t, preBurn, rebuilt, "the re-render overwrites the delivered report at the same path")

	after, err := os.ReadFile(rebuilt)
	require.NoError(t, err)
	assert.NotEqual(t, before, after, "the re-rendered report must differ once it records the burned discs")
}

// TestQueryDeviceIdentityDegrades covers the graceful-degradation contract: when
// the library exposes no drives or changer to query (e.g. a dry run with no
// device), every hardware identifier degrades to "unknown" rather than failing.
func TestQueryDeviceIdentityDegrades(t *testing.T) {
	t.Parallel()

	device := queryDeviceIdentity(t.Context(), config.Library{})

	assert.Equal(t, unknownIdentity, device.driveModel)
	assert.Equal(t, unknownIdentity, device.driveSerial)
	assert.Equal(t, unknownIdentity, device.driveGeneration)
	assert.Equal(t, unknownIdentity, device.libraryModel)
}

// TestArchiveName covers the display name derived from each kind of config Source.
func TestArchiveName(t *testing.T) {
	t.Parallel()

	cfg := config.Config{Sources: []config.Source{
		{ZFSPath: &config.ZFSPathSource{Name: "pool/data@snap"}},
		{K8s: &config.K8sRef{Namespace: "plex", Name: "plex-snap"}},
		{K8s: &config.K8sRef{Namespace: "default", LabelSelector: "app=web"}},
	}}

	assert.Equal(t, "pool/data@snap", archiveName(cfg, 0))
	assert.Equal(t, "plex/plex-snap", archiveName(cfg, 1))
	assert.Equal(t, "default [app=web]", archiveName(cfg, 2))
	assert.Equal(t, "sources[9]", archiveName(cfg, 9))
}

// TestRedundancyPolicy covers both PAR2 modes and the empty fallback.
func TestRedundancyPolicy(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "10%", redundancyPolicy(config.Redundancy{TargetPercentage: ptrFloat(10)}))
	assert.Equal(t, "fill-to-capacity (floor 5%)", redundancyPolicy(config.Redundancy{FillToCapacity: &config.FillConfig{Floor: 5}}))
	assert.Equal(t, "none", redundancyPolicy(config.Redundancy{}))
}

// TestLTOGeneration covers the capacity→generation mapping that classifies the
// write-health speed-matching floor, including the below-range fallback.
func TestLTOGeneration(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		capacity int64
		want     string
	}{
		"LTO-6": {2_500_000_000_000, "LTO-6"},
		"LTO-7": {6_000_000_000_000, "LTO-7"},
		"LTO-8": {12_000_000_000_000, "LTO-8"},
		"LTO-9": {18_000_000_000_000, "LTO-9"},
		"tiny":  {1000, "unknown (native capacity 1000 bytes)"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, ltoGeneration(test.capacity))
		})
	}
}

// TestBuildSHA256Manifest checks the manifest lists every on-tape file for every
// physical tape in sorted, sha256sum-format lines prefixed by the barcode.
func TestBuildSHA256Manifest(t *testing.T) {
	t.Parallel()

	manifest, err := buildSHA256Manifest(reportTestInput(t))
	require.NoError(t, err)

	text := string(manifest)
	// 3 files × 2 tapes = 6 lines.
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	assert.Len(t, lines, 6)
	assert.Contains(t, text, "aa  TAPE01L6/archives/000-archive/archive.000")
	assert.Contains(t, text, "cc  TAPE02L6/archives/000-archive/archive.par2")
	// Sorted output.
	assert.True(t, sortedStrings(lines), "manifest lines must be sorted")
}

// TestRecoveryProcedureSHA256Layout proves the recovery instructions' verification
// step actually works: it builds a real manifest.sha256, lays the copied files out
// exactly as the concise procedure (report.go recoveryProcedure) and the operator
// doc describe — <barcode>/archives/NNN-<label>/<file>, relative to a copy root —
// and runs the system sha256sum against it. It covers the full-copy case and the
// documented single-tape (grep-by-barcode) partial-copy case.
func TestRecoveryProcedureSHA256Layout(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available")
	}

	// Content keyed by on-tape file basename. Both physical tapes carry the same
	// files (copies), so one content map covers every manifest line.
	contents := map[string][]byte{
		"archive.000":  []byte("slice zero contents"),
		"archive.001":  []byte("slice one contents"),
		"archive.par2": []byte("par2 recovery set"),
	}

	digest := func(content []byte) string {
		sum := sha256.Sum256(content)
		return hex.EncodeToString(sum[:])
	}

	input := reportTestInput(t)
	input.Staged[0].Slices = []StagedSlice{
		{Path: "/stage/000/archive.000", SHA256: digest(contents["archive.000"]), SizeBytes: int64(len(contents["archive.000"]))},
		{Path: "/stage/000/archive.001", SHA256: digest(contents["archive.001"]), SizeBytes: int64(len(contents["archive.001"]))},
	}
	input.PAR2[0].Files = []StagedSlice{
		{Path: "/stage/000/archive.par2", SHA256: digest(contents["archive.par2"]), SizeBytes: int64(len(contents["archive.par2"]))},
	}

	manifest, err := buildSHA256Manifest(input)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(string(manifest), "\n"), "\n")

	// writeLayout materializes the manifest's paths under root (a copy root), so
	// each file sits at <root>/<barcode>/archives/NNN-<label>/<file> — the layout
	// the instructions tell a recoverer to create. keep selects which tapes' files
	// to write, modelling a full vs. partial copy.
	writeLayout := func(root string, keep func(relPath string) bool) {
		for _, line := range lines {
			parts := strings.SplitN(line, "  ", 2)
			require.Len(t, parts, 2, "manifest line %q is not <digest>  <path>", line)

			relPath := parts[1]
			if !keep(relPath) {
				continue
			}

			content, ok := contents[filepath.Base(relPath)]
			require.True(t, ok, "no content for on-tape file %q", relPath)

			full := filepath.Join(root, relPath)
			require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
			require.NoError(t, os.WriteFile(full, content, 0o644))
		}
	}

	runCheck := func(root string, checklist []byte) error {
		cmd := exec.Command("sha256sum", "-c", "-")
		cmd.Dir = root
		cmd.Stdin = bytes.NewReader(checklist)

		return cmd.Run()
	}

	// Full copy: every tape's files are present, so the whole manifest verifies.
	allRoot := t.TempDir()
	writeLayout(allRoot, func(string) bool { return true })
	assert.NoError(t, runCheck(allRoot, manifest),
		"files laid out per the recovery instructions must verify against the full manifest")

	// Partial copy: only TAPE01L6's files present. Checking the full manifest
	// fails on the not-yet-copied tape's lines — the exact confusion the fixed
	// instructions warn about — but the documented per-tape barcode filter verifies
	// the copied tape.
	partialRoot := t.TempDir()
	writeLayout(partialRoot, func(relPath string) bool {
		return strings.HasPrefix(relPath, "TAPE01L6/")
	})
	assert.Error(t, runCheck(partialRoot, manifest),
		"the full manifest must fail when only some tapes have been copied")

	var filtered []string

	for _, line := range lines {
		if strings.Contains(line, "  TAPE01L6/") {
			filtered = append(filtered, line)
		}
	}

	require.NotEmpty(t, filtered, "expected at least one line for the copied tape")
	assert.NoError(t, runCheck(partialRoot, []byte(strings.Join(filtered, "\n")+"\n")),
		"filtering the manifest to the copied tape's barcode must verify the partial copy")
}

func sortedStrings(lines []string) bool {
	for index := 1; index < len(lines); index++ {
		if lines[index-1] > lines[index] {
			return false
		}
	}

	return true
}

// TestVerifyEscrowIdentity checks the escrow contract against the real age-keygen:
// a generated identity whose recipient is configured passes; an empty identity or
// one whose recipient is not configured fails.
func TestVerifyEscrowIdentity(t *testing.T) {
	t.Parallel()

	identity, recipient := generateTestKeypair(t)

	t.Run("matching identity passes", func(t *testing.T) {
		t.Parallel()
		err := verifyEscrowIdentity(t.Context(), config.Encryption{
			Recipients: []string{recipient},
			Identity:   identity,
		})
		require.NoError(t, err)
	})

	// AC1: an empty escrow identity is a deterministic misconfiguration; the error
	// must be non-retryable so the Report phase fails promptly (and the SPEC §11
	// alert fires) instead of retrying the activity forever.
	t.Run("empty identity fails non-retryably", func(t *testing.T) {
		t.Parallel()
		err := verifyEscrowIdentity(t.Context(), config.Encryption{Recipients: []string{recipient}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
		assertNonRetryable(t, err)
	})

	// AC1: a rotated or wrong identity that derives to a non-configured recipient
	// is likewise deterministic; the mismatch must fail non-retryably.
	t.Run("unmatched recipient fails non-retryably", func(t *testing.T) {
		t.Parallel()
		err := verifyEscrowIdentity(t.Context(), config.Encryption{
			Recipients: []string{"age1pq1someoneelse"},
			Identity:   identity,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not among the configured")
		assertNonRetryable(t, err)
	})
}

// assertNonRetryable asserts err is a Temporal ApplicationError marked
// non-retryable, so the server-default unlimited retry policy does not loop on it.
func assertNonRetryable(t *testing.T, err error) {
	t.Helper()

	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr, "error must be a Temporal ApplicationError")
	assert.True(t, appErr.NonRetryable(), "error must be marked non-retryable")
}

// TestBuildReport covers AC4: with optical burning disabled the activity produces
// only the PDF report — no recovery ISO, compressed or uncompressed, and no disc
// manifest — in the staging directory.
func TestBuildReport(t *testing.T) {
	t.Parallel()

	identity, recipient := generateTestKeypair(t)

	input := reportTestInput(t)
	input.Config.Encryption = config.Encryption{Recipients: []string{recipient}, Identity: identity}

	// Stage real slice/PAR2 files so the report references existing base names
	// (buildReport itself does not read them, but this mirrors a real run).
	stageDir := t.TempDir()
	input.Staged[0].Slices = []StagedSlice{
		{Path: filepath.Join(stageDir, "archive.000"), SHA256: "aa", SizeBytes: 3},
		{Path: filepath.Join(stageDir, "archive.001"), SHA256: "bb", SizeBytes: 3},
	}
	input.PAR2[0].Files = []StagedSlice{{Path: filepath.Join(stageDir, "archive.par2"), SHA256: "cc", SizeBytes: 3}}

	acts := newReportActivities(t.TempDir(), testutil.RecoveryBinariesDir(t), testutil.RecoverySourcesDir(t))

	outDir := t.TempDir()

	output, err := acts.buildReport(t.Context(), outDir, input)
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(outDir, reportFileName), output.ReportPath)

	// Optical burning is disabled (no delivery.opticalBurn section), so no recovery
	// ISO — compressed or uncompressed — and no disc manifest are produced.
	assert.Empty(t, output.UncompressedISOPath, "no uncompressed ISO must be staged when burning is disabled")
	assert.NoFileExists(t, filepath.Join(outDir, isoFileName), "the uncompressed ISO file must not exist when burning is disabled")
	assert.NoFileExists(t, filepath.Join(outDir, "recovery.iso.zst"), "no compressed ISO must ever be produced")
	assert.Empty(t, output.DiscManifestPath, "no disc-content manifest must be staged when burning is disabled")
	assert.NoFileExists(t, filepath.Join(outDir, discManifestFileName), "the disc-content manifest must not exist when burning is disabled")

	// Only the report is staged in the output directory.
	entries, err := os.ReadDir(outDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the PDF report must be produced when burning is disabled")
	assert.Equal(t, reportFileName, entries[0].Name())

	pdf, err := os.ReadFile(output.ReportPath)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(pdf), "%PDF-"), "report must be a PDF")
}

// TestBuildReportStagesUncompressedISOWhenBurning covers AC5: with optical burning
// enabled the Report phase stages the uncompressed recovery ISO beside the run
// artifacts and records its path, the staged image is a valid ISO 9660, and no
// compressed .zst ISO is produced.
func TestBuildReportStagesUncompressedISOWhenBurning(t *testing.T) {
	t.Parallel()

	identity, recipient := generateTestKeypair(t)

	input := reportTestInput(t)
	input.Config.Encryption = config.Encryption{Recipients: []string{recipient}, Identity: identity}
	input.Config.Delivery.OpticalBurn = &config.OpticalBurn{Drives: []string{"/dev/sr0"}, Copies: 1}

	acts := newReportActivities(t.TempDir(), testutil.RecoveryBinariesDir(t), testutil.RecoverySourcesDir(t))

	outDir := t.TempDir()

	output, err := acts.buildReport(t.Context(), outDir, input)
	require.NoError(t, err)

	// The uncompressed ISO is staged and its path recorded for the Burn phase.
	assert.Equal(t, filepath.Join(outDir, isoFileName), output.UncompressedISOPath)

	staged, err := os.ReadFile(output.UncompressedISOPath)
	require.NoError(t, err)
	assertValidISO9660(t, staged)

	// No compressed ISO is produced.
	assert.NoFileExists(t, filepath.Join(outDir, "recovery.iso.zst"), "no compressed ISO must be produced")
}

// assertValidISO9660 checks that data is an ISO 9660 image: its Primary Volume
// Descriptor carries the "CD001" standard identifier at the start of logical
// sector 16 (byte offset 32769, after the 32768-byte system area and the 1-byte
// volume-descriptor type).
func assertValidISO9660(t *testing.T, data []byte) {
	t.Helper()

	const pvdIdentifierOffset = 32769

	require.GreaterOrEqual(t, len(data), pvdIdentifierOffset+5, "image too small to hold an ISO 9660 volume descriptor")
	assert.Equal(t, "CD001", string(data[pvdIdentifierOffset:pvdIdentifierOffset+5]), "image must carry the ISO 9660 CD001 identifier")
}

// TestBuildReportRejectsBadIdentity checks the activity fails before writing any
// artifact when the escrow identity does not match a recipient.
func TestBuildReportRejectsBadIdentity(t *testing.T) {
	t.Parallel()

	identity, _ := generateTestKeypair(t)

	input := reportTestInput(t)
	input.Config.Encryption = config.Encryption{Recipients: []string{"age1pq1wrong"}, Identity: identity}

	acts := newReportActivities(t.TempDir(), testutil.RecoveryBinariesDir(t), testutil.RecoverySourcesDir(t))

	outDir := t.TempDir()

	_, err := acts.buildReport(t.Context(), outDir, input)
	require.Error(t, err)

	entries, readErr := os.ReadDir(outDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "no artifacts must be written when identity verification fails")
}

// generateTestKeypair generates a fresh age post-quantum keypair with age-keygen
// and returns the identity file contents and the recipient.
func generateTestKeypair(t *testing.T) (identity, recipient string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "identity.txt")

	require.NoError(t, exec.CommandContext(t.Context(), "age-keygen", "-pq", "-o", path).Run(), "age-keygen")

	contents, err := os.ReadFile(path)
	require.NoError(t, err)

	const marker = "# public key: "

	for _, line := range strings.Split(string(contents), "\n") {
		if after, found := strings.CutPrefix(line, marker); found {
			recipient = strings.TrimSpace(after)

			break
		}
	}

	require.NotEmpty(t, recipient, "recipient not found in identity file")

	return string(contents), recipient
}

// TestTapeIndexesReadFromStagedFiles pins the disk-staging fix for issue #221:
// the recovery-ISO tape indexes are read from the paths FinalizeTape staged them
// to, not carried in the ReportInput payload. This is what keeps the post-write
// Report payload bounded regardless of run size (no multi-MB index per copy).
func TestTapeIndexesReadFromStagedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pathA := filepath.Join(dir, "TAPE01L6.xml")
	pathB := filepath.Join(dir, "TAPE02L6.xml")

	require.NoError(t, os.WriteFile(pathA, []byte("<ltfsindex>A</ltfsindex>"), 0o644))
	require.NoError(t, os.WriteFile(pathB, []byte("<ltfsindex>B</ltfsindex>"), 0o644))

	written := []WrittenTape{
		{Barcode: tape.Barcode("TAPE01L6"), IndexXMLPath: pathA},
		{Barcode: tape.Barcode("TAPE02L6"), IndexXMLPath: pathB},
	}

	indexes, err := tapeIndexes(written)
	require.NoError(t, err)

	require.Len(t, indexes, 2)
	assert.Equal(t, "TAPE01L6", indexes[0].Barcode)
	assert.Equal(t, []byte("<ltfsindex>A</ltfsindex>"), indexes[0].Index)
	assert.Equal(t, "TAPE02L6", indexes[1].Barcode)
	assert.Equal(t, []byte("<ltfsindex>B</ltfsindex>"), indexes[1].Index)
}

// TestTapeIndexesMissingFileErrors ensures a missing staged index is a named,
// actionable error rather than a silent empty index in the recovery ISO.
func TestTapeIndexesMissingFileErrors(t *testing.T) {
	t.Parallel()

	written := []WrittenTape{
		{Barcode: tape.Barcode("TAPE01L6"), IndexXMLPath: filepath.Join(t.TempDir(), "absent.xml")},
	}

	_, err := tapeIndexes(written)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TAPE01L6", "the error must name the tape whose staged index is missing")
}
