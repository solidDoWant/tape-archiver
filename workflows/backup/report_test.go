package backup

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
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
	"github.com/solidDoWant/tape-archiver/pkg/archive"
	"github.com/solidDoWant/tape-archiver/pkg/optical"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// reportTestInput builds one raw-ZFS archive (two slices, one PAR2 file) on one
// logical tape written as two copies.
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

// stageTestIndex stages a captured LTFS index to disk, as FinalizeTape does (#221).
func stageTestIndex(t *testing.T, barcode string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), barcode+".xml")
	require.NoError(t, os.WriteFile(path, []byte("<ltfsindex/>"), 0o644))

	return path
}

func ptrFloat(f float64) *float64 { return &f }

// TestBuildReportManifest checks the run state maps into every SPEC §9 field.
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
	require.Len(t, archive.Files, 3)
	assert.Equal(t, "archive.000", archive.Files[0].Name)
	assert.Equal(t, "archive.par2", archive.Files[2].Name)
	assert.Equal(t, "cc", archive.Files[2].SHA256)

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

// TestReportTapesPropagatesOverwroteNonBlank: only an overwriting tape carries the
// flag in the report.
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

// TestReportDiscsPropagatesOverwrite: burned discs map into the report; a run
// without burning has no Discs section.
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

// TestBuildReportStagesDiscManifest: with burning enabled the disc-content manifest
// of the ISO's own files is staged and its path recorded.
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

	for _, discPath := range []string{"report.pdf", "manifest.sha256", "recovery-procedure.md"} {
		assert.Contains(t, manifest, discPath, "disc manifest must list the ISO's own %s", discPath)
	}

	assert.Contains(t, manifest, "ltfs-index/tape01l6.schema",
		"disc manifest must list each tape's LTFS index backup at its lowercased read-back path")

	for discPath, digest := range manifest {
		assert.Lenf(t, digest, 64, "digest for %s must be a hex SHA-256", discPath)
	}
}

// TestRebuildDeliveredReport: the post-burn re-render overwrites report.pdf in
// place, recording the burned discs.
func TestRebuildDeliveredReport(t *testing.T) {
	t.Parallel()

	identity, recipient := generateTestKeypair(t)

	input := reportTestInput(t)
	input.Config.Encryption = config.Encryption{Recipients: []string{recipient}, Identity: identity}

	acts := newReportActivities(t.TempDir(), testutil.RecoveryBinariesDir(t), testutil.RecoverySourcesDir(t))
	outDir := t.TempDir()

	preBurn, err := acts.rebuildReport(t.Context(), outDir, input)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(outDir, reportFileName), preBurn)

	before, err := os.ReadFile(preBurn)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(before), "%PDF-"), "delivered report must be a PDF")

	input.Discs = []BurnResult{{Device: "/dev/sr0", OverwroteNonBlank: true}}

	rebuilt, err := acts.rebuildReport(t.Context(), outDir, input)
	require.NoError(t, err)
	assert.Equal(t, preBurn, rebuilt, "the re-render overwrites the delivered report at the same path")

	after, err := os.ReadFile(rebuilt)
	require.NoError(t, err)
	assert.NotEqual(t, before, after, "the re-rendered report must differ once it records the burned discs")
}

// TestQueryDeviceIdentityDegrades: with nothing to query, identifiers degrade to
// "unknown" rather than failing.
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

// TestLTOGeneration covers the capacity→generation mapping, including the
// below-range fallback.
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

// TestBuildSHA256Manifest checks sorted, barcode-prefixed sha256sum lines for every
// on-tape file of every tape.
func TestBuildSHA256Manifest(t *testing.T) {
	t.Parallel()

	manifest, err := buildSHA256Manifest(reportTestInput(t))
	require.NoError(t, err)

	text := string(manifest)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	assert.Len(t, lines, 6)
	assert.Contains(t, text, "aa  TAPE01L6/archives/000-archive/archive.000")
	assert.Contains(t, text, "cc  TAPE02L6/archives/000-archive/archive.par2")
	assert.True(t, sortedStrings(lines), "manifest lines must be sorted")
}

// TestTapeGeneration (issue #340): the report names the media generation, read
// from each tape, never inferred from the writing drive.
func TestTapeGeneration(t *testing.T) {
	t.Parallel()

	writtenTape := func(generation string) WrittenTape {
		return WrittenTape{MediaGeneration: generation}
	}

	tests := map[string]struct {
		written []WrittenTape
		want    string
	}{
		"all tapes one generation": {
			written: []WrittenTape{writtenTape("LTO-5"), writtenTape("LTO-5")},
			want:    "LTO-5",
		},
		// Naming one generation would be wrong for the other tapes it covers.
		"generations differ": {
			written: []WrittenTape{writtenTape("LTO-5"), writtenTape("LTO-6")},
			want:    "mixed",
		},
		// One unreadable medium makes the whole answer unknown.
		"one tape unreadable": {
			written: []WrittenTape{writtenTape("LTO-5"), writtenTape("unknown")},
			want:    "unknown",
		},
		"never measured": {
			written: []WrittenTape{writtenTape("")},
			want:    "unknown",
		},
		"no tapes": {want: "unknown"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, tapeGeneration(test.written))
		})
	}
}

// TestManifestCarriesPerArchiveCompression (issue #340): each archive carries its
// own source's compression setting.
func TestManifestCarriesPerArchiveCompression(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{"compressed": true, "uncompressed": false}

	for name, compression := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			input := reportTestInput(t)
			input.Resolved[0].Compression = compression

			manifest := buildReportManifest(input, deviceIdentity{})

			require.Len(t, manifest.Archives, 1)
			assert.Equal(t, compression, manifest.Archives[0].Compressed,
				"the archive must report its own source's compression setting")
		})
	}
}

// TestRecoveryProcedureMountStepNamesADevice (issue #340): "ltfs -o ro" alone opens
// no device, and LTFS's sg backend needs /dev/sgN, not /dev/nstN, so the printed
// step must carry -o devname=.
func TestRecoveryProcedureMountStepNamesADevice(t *testing.T) {
	t.Parallel()

	assert.Contains(t, recoveryProcedure, "ltfs -o ro -o devname=/dev/sgN <mountpoint>",
		"the mount step must name the device LTFS is to open")
	assert.Contains(t, recoveryProcedure, "lsscsi -g",
		"the mount step must say how to find the drive's sg node")
}

// TestRecoveryProcedureUnpackStepMatchesTheTar (issue #340): the printed unpack
// step must agree with the tar layout tarMembers actually writes.
func TestRecoveryProcedureUnpackStepMatchesTheTar(t *testing.T) {
	t.Parallel()

	member := func(subdir, file string) archive.Member {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte("contents"), 0o644))

		return archive.Member{Subdir: subdir, Dir: dir}
	}

	entries := func(members ...archive.Member) []string {
		var buf bytes.Buffer

		require.NoError(t, tarMembers(t.Context(), &buf, members))

		var names []string

		reader := tar.NewReader(&buf)

		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}

			require.NoError(t, err)

			names = append(names, header.Name)
		}

		return names
	}

	// A single member unpacks with no wrapping volume directory.
	single := entries(member("volume-a", "only.txt"))
	assert.Contains(t, single, "only.txt")

	for _, name := range single {
		assert.NotContains(t, name, "volume-a/",
			"a single-volume archive must unpack with no wrapping volume directory")
	}

	group := entries(member("volume-a", "a.txt"), member("volume-b", "b.txt"))
	assert.Contains(t, group, "volume-a/a.txt")
	assert.Contains(t, group, "volume-b/b.txt")
}

// TestRecoveryProcedureSHA256Layout runs the system sha256sum against a real
// manifest laid out exactly as the recovery procedure describes, for both a full
// copy and the documented single-tape partial copy.
func TestRecoveryProcedureSHA256Layout(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available")
	}

	// Both tapes carry the same files, so one content map covers every line.
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

	// writeLayout writes the manifest's files under root; keep selects which tapes.
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

	allRoot := t.TempDir()
	writeLayout(allRoot, func(string) bool { return true })
	assert.NoError(t, runCheck(allRoot, manifest),
		"files laid out per the recovery instructions must verify against the full manifest")

	// Partial copy: the full manifest fails on the missing tape, but the documented
	// per-tape barcode filter verifies the copied one.
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

	t.Run("empty identity fails non-retryably", func(t *testing.T) {
		t.Parallel()
		err := verifyEscrowIdentity(t.Context(), config.Encryption{Recipients: []string{recipient}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
		assertNonRetryable(t, err)
	})

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

// assertNonRetryable asserts err is a non-retryable Temporal ApplicationError.
func assertNonRetryable(t *testing.T, err error) {
	t.Helper()

	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr, "error must be a Temporal ApplicationError")
	assert.True(t, appErr.NonRetryable(), "error must be marked non-retryable")
}

// TestBuildReport: with burning disabled only the PDF report is produced.
func TestBuildReport(t *testing.T) {
	t.Parallel()

	identity, recipient := generateTestKeypair(t)

	input := reportTestInput(t)
	input.Config.Encryption = config.Encryption{Recipients: []string{recipient}, Identity: identity}

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

	assert.Empty(t, output.UncompressedISOPath, "no uncompressed ISO must be staged when burning is disabled")
	assert.NoFileExists(t, filepath.Join(outDir, isoFileName), "the uncompressed ISO file must not exist when burning is disabled")
	assert.NoFileExists(t, filepath.Join(outDir, "recovery.iso.zst"), "no compressed ISO must ever be produced")
	assert.Empty(t, output.DiscManifestPath, "no disc-content manifest must be staged when burning is disabled")
	assert.NoFileExists(t, filepath.Join(outDir, discManifestFileName), "the disc-content manifest must not exist when burning is disabled")

	entries, err := os.ReadDir(outDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the PDF report must be produced when burning is disabled")
	assert.Equal(t, reportFileName, entries[0].Name())

	pdf, err := os.ReadFile(output.ReportPath)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(pdf), "%PDF-"), "report must be a PDF")
}

// TestBuildReportStagesUncompressedISOWhenBurning: with burning enabled a valid
// uncompressed ISO 9660 is staged and no .zst is produced.
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

	assert.Equal(t, filepath.Join(outDir, isoFileName), output.UncompressedISOPath)

	staged, err := os.ReadFile(output.UncompressedISOPath)
	require.NoError(t, err)
	assertValidISO9660(t, staged)

	assert.NoFileExists(t, filepath.Join(outDir, "recovery.iso.zst"), "no compressed ISO must be produced")
}

// assertValidISO9660 checks for the "CD001" identifier of the Primary Volume
// Descriptor at byte 32769 (sector 16).
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

// TestTapeIndexesReadFromStagedFiles (issue #221): tape indexes are read from the
// staged files, keeping the Report payload bounded.
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

// TestTapeIndexesMissingFileErrors: a missing staged index is a named error, not a
// silent empty index.
func TestTapeIndexesMissingFileErrors(t *testing.T) {
	t.Parallel()

	written := []WrittenTape{
		{Barcode: tape.Barcode("TAPE01L6"), IndexXMLPath: filepath.Join(t.TempDir(), "absent.xml")},
	}

	_, err := tapeIndexes(written)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TAPE01L6", "the error must name the tape whose staged index is missing")
}
