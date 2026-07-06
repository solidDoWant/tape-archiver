package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

func TestBuildManifest(t *testing.T) {
	t.Parallel()

	archives := []TapeWriteArchive{
		{
			SourceIndex: 0,
			Label:       "photos",
			Slices: []StagedSlice{
				{Path: "/staging/archive.000", SHA256: "aaa111", SizeBytes: 1024},
				{Path: "/staging/archive.001", SHA256: "bbb222", SizeBytes: 512},
			},
			PAR2Files: []StagedSlice{
				{Path: "/staging/archive.par2", SHA256: "ccc333", SizeBytes: 128},
			},
		},
		{
			SourceIndex: 2,
			Label:       "plex-group-snap",
			Slices: []StagedSlice{
				{Path: "/staging/other.000", SHA256: "ddd444", SizeBytes: 2048},
			},
			PAR2Files: nil,
		},
	}

	got := buildManifest("TAPE01L8", 1, 0, archives)

	assert.Equal(t, tape.Barcode("TAPE01L8"), got.Barcode)
	assert.Equal(t, 1, got.TapeIndex)
	assert.Equal(t, 0, got.CopyIndex)
	require.Len(t, got.Archives, 2)

	arch0 := got.Archives[0]
	assert.Equal(t, 0, arch0.SourceIndex)
	require.Len(t, arch0.Files, 2)
	assert.Equal(t, "archives/000-photos/archive.000", arch0.Files[0].TapePath)
	assert.Equal(t, "aaa111", arch0.Files[0].SHA256)
	assert.Equal(t, int64(1024), arch0.Files[0].SizeBytes)
	assert.Equal(t, "archives/000-photos/archive.001", arch0.Files[1].TapePath)
	require.Len(t, arch0.PAR2Files, 1)
	assert.Equal(t, "archives/000-photos/archive.par2", arch0.PAR2Files[0].TapePath)
	assert.Equal(t, "ccc333", arch0.PAR2Files[0].SHA256)

	arch2 := got.Archives[1]
	assert.Equal(t, 2, arch2.SourceIndex)
	require.Len(t, arch2.Files, 1)
	assert.Equal(t, "archives/002-plex-group-snap/other.000", arch2.Files[0].TapePath)
	assert.Empty(t, arch2.PAR2Files)
}

// TestBuildManifestSharedLabel proves that when two sources carry the same
// descriptive label, the NNN source-index prefix keeps their on-tape directories
// distinct so neither archive's manifest paths collide with the other's.
func TestBuildManifestSharedLabel(t *testing.T) {
	t.Parallel()

	archives := []TapeWriteArchive{
		{
			SourceIndex: 0,
			Label:       "archive",
			Slices:      []StagedSlice{{Path: "/staging/archive.000", SHA256: "aaa", SizeBytes: 1}},
		},
		{
			SourceIndex: 1,
			Label:       "archive",
			Slices:      []StagedSlice{{Path: "/staging/archive.000", SHA256: "bbb", SizeBytes: 1}},
		},
	}

	got := buildManifest("TAPE01L8", 0, 0, archives)
	require.Len(t, got.Archives, 2)

	first := got.Archives[0].Files[0].TapePath
	second := got.Archives[1].Files[0].TapePath

	assert.Equal(t, "archives/000-archive/archive.000", first)
	assert.Equal(t, "archives/001-archive/archive.000", second)
	assert.NotEqual(t, first, second, "shared label must not collapse distinct archives onto one path")
}

func TestBuildManifestEmpty(t *testing.T) {
	t.Parallel()

	got := buildManifest("EMPTY01L8", 0, 0, nil)

	assert.Equal(t, tape.Barcode("EMPTY01L8"), got.Barcode)
	assert.Empty(t, got.Archives)
}

func TestWriteManifest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	manifest := TapeManifest{
		Barcode:   "TAPE01L8",
		TapeIndex: 0,
		CopyIndex: 1,
		Archives: []ArchiveManifest{
			{
				SourceIndex: 0,
				Files: []ManifestFile{
					{TapePath: "archives/000-photos/archive.000", SHA256: "abc123", SizeBytes: 100},
				},
				PAR2Files: []ManifestFile{
					{TapePath: "archives/000-photos/archive.par2", SHA256: "def456", SizeBytes: 10},
				},
			},
		},
	}

	require.NoError(t, writeManifest(dir, manifest))

	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	require.NoError(t, err)

	var got TapeManifest
	require.NoError(t, json.Unmarshal(data, &got))

	assert.Equal(t, tape.Barcode("TAPE01L8"), got.Barcode)
	assert.Equal(t, 0, got.TapeIndex)
	assert.Equal(t, 1, got.CopyIndex)
	require.Len(t, got.Archives, 1)
	assert.Equal(t, 0, got.Archives[0].SourceIndex)
	require.Len(t, got.Archives[0].Files, 1)
	assert.Equal(t, "archives/000-photos/archive.000", got.Archives[0].Files[0].TapePath)
}

func TestCopyTape(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := t.TempDir()

	// Create staged slice and PAR2 files.
	slice0 := filepath.Join(src, "archive.000")
	require.NoError(t, os.WriteFile(slice0, []byte("slice0-content"), 0o644))

	slice1 := filepath.Join(src, "archive.001")
	require.NoError(t, os.WriteFile(slice1, []byte("slice1-content"), 0o644))

	par2 := filepath.Join(src, "archive.par2")
	require.NoError(t, os.WriteFile(par2, []byte("par2-content"), 0o644))

	archives := []TapeWriteArchive{
		{
			SourceIndex: 0,
			Label:       "photos",
			Slices: []StagedSlice{
				{Path: slice0, SHA256: "ignored", SizeBytes: 14},
				{Path: slice1, SHA256: "ignored", SizeBytes: 14},
			},
			PAR2Files: []StagedSlice{
				{Path: par2, SHA256: "ignored", SizeBytes: 12},
			},
		},
	}

	require.NoError(t, copyTape(t.Context(), dst, archives))

	// Verify the on-tape directory layout: archives/000-<label>/<files>
	archDir := filepath.Join(dst, "archives", "000-photos")
	entries, err := os.ReadDir(archDir)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	gotSlice0, err := os.ReadFile(filepath.Join(archDir, "archive.000"))
	require.NoError(t, err)
	assert.Equal(t, "slice0-content", string(gotSlice0))

	gotSlice1, err := os.ReadFile(filepath.Join(archDir, "archive.001"))
	require.NoError(t, err)
	assert.Equal(t, "slice1-content", string(gotSlice1))

	gotPAR2, err := os.ReadFile(filepath.Join(archDir, "archive.par2"))
	require.NoError(t, err)
	assert.Equal(t, "par2-content", string(gotPAR2))
}

func TestCopyTapeMultipleArchives(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := t.TempDir()

	file0 := filepath.Join(src, "a0.000")
	require.NoError(t, os.WriteFile(file0, []byte("archive0"), 0o644))

	file2 := filepath.Join(src, "a2.000")
	require.NoError(t, os.WriteFile(file2, []byte("archive2"), 0o644))

	archives := []TapeWriteArchive{
		{SourceIndex: 0, Label: "photos", Slices: []StagedSlice{{Path: file0}}},
		{SourceIndex: 2, Label: "videos", Slices: []StagedSlice{{Path: file2}}},
	}

	require.NoError(t, copyTape(t.Context(), dst, archives))

	// Source index 0 → archives/000-photos, source index 2 → archives/002-videos
	got0, err := os.ReadFile(filepath.Join(dst, "archives", "000-photos", "a0.000"))
	require.NoError(t, err)
	assert.Equal(t, "archive0", string(got0))

	got2, err := os.ReadFile(filepath.Join(dst, "archives", "002-videos", "a2.000"))
	require.NoError(t, err)
	assert.Equal(t, "archive2", string(got2))
}

// TestCopyTapeSharedLabel proves that two sources whose labels are identical (by
// override, by derivation, or by sanitizing to the same string) still land in
// distinct on-tape directories — the NNN prefix disambiguates — so neither
// overwrites the other on the mounted volume, even when their slice basenames match.
func TestCopyTapeSharedLabel(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := t.TempDir()

	// Both archives use the identical slice basename (archive.000) and label, so a
	// missing NNN prefix would make the second copy clobber the first.
	file0 := filepath.Join(src, "s0", "archive.000")
	require.NoError(t, os.MkdirAll(filepath.Dir(file0), 0o755))
	require.NoError(t, os.WriteFile(file0, []byte("first-source"), 0o644))

	file1 := filepath.Join(src, "s1", "archive.000")
	require.NoError(t, os.MkdirAll(filepath.Dir(file1), 0o755))
	require.NoError(t, os.WriteFile(file1, []byte("second-source"), 0o644))

	archives := []TapeWriteArchive{
		{SourceIndex: 0, Label: "archive", Slices: []StagedSlice{{Path: file0}}},
		{SourceIndex: 1, Label: "archive", Slices: []StagedSlice{{Path: file1}}},
	}

	require.NoError(t, copyTape(t.Context(), dst, archives))

	got0, err := os.ReadFile(filepath.Join(dst, "archives", "000-archive", "archive.000"))
	require.NoError(t, err)
	assert.Equal(t, "first-source", string(got0))

	got1, err := os.ReadFile(filepath.Join(dst, "archives", "001-archive", "archive.000"))
	require.NoError(t, err)
	assert.Equal(t, "second-source", string(got1), "the shared-label archive must not be overwritten")
}

func TestCopyTapeCancelledContext(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := t.TempDir()

	file := filepath.Join(src, "archive.000")
	require.NoError(t, os.WriteFile(file, []byte("content"), 0o644))

	archives := []TapeWriteArchive{
		{SourceIndex: 0, Slices: []StagedSlice{{Path: file}}},
	}

	// A pre-cancelled context causes copyTape to abort on the context check
	// between files. With a single file the check fires before copying it.
	cancelledCtx, cancel := context.WithCancel(t.Context())
	cancel()

	err := copyTape(cancelledCtx, dst, archives)
	require.Error(t, err, "cancelled context must cause copyTape to abort")
}

func TestArchivesForTape(t *testing.T) {
	t.Parallel()

	state := &runState{
		resolved: []ResolvedArchive{
			{SourceIndex: 0, Label: "photos"},
			{SourceIndex: 1, Label: "videos"},
			{SourceIndex: 2, Label: "plex-group-snap"},
		},
		plan: TapePlan{
			Copies: 1,
			Tapes: []PlannedTape{
				{
					Archives: []PlannedArchive{
						{SourceIndex: 0, DataBytes: 1024},
						{SourceIndex: 1, DataBytes: 2048},
					},
				},
				{
					Archives: []PlannedArchive{
						{SourceIndex: 2, DataBytes: 512},
					},
				},
			},
		},
		staged: []StagedArchive{
			{SourceIndex: 0, Slices: []StagedSlice{{Path: "/staging/a0.000", SHA256: "aaa", SizeBytes: 1024}}},
			{SourceIndex: 1, Slices: []StagedSlice{{Path: "/staging/a1.000", SHA256: "bbb", SizeBytes: 2048}}},
			{SourceIndex: 2, Slices: []StagedSlice{{Path: "/staging/a2.000", SHA256: "ccc", SizeBytes: 512}}},
		},
		par2: []PAR2Set{
			{SourceIndex: 0, Files: []StagedSlice{{Path: "/staging/a0.par2", SHA256: "ddd", SizeBytes: 100}}},
			{SourceIndex: 1, Files: []StagedSlice{{Path: "/staging/a1.par2", SHA256: "eee", SizeBytes: 200}}},
			{SourceIndex: 2, Files: []StagedSlice{{Path: "/staging/a2.par2", SHA256: "fff", SizeBytes: 50}}},
		},
	}

	tests := []struct {
		name       string
		tapeIndex  int
		wantLen    int
		wantSI     []int
		wantLabels []string
		wantErr    require.ErrorAssertionFunc
	}{
		{
			name:       "tape 0 gets archives 0 and 1",
			tapeIndex:  0,
			wantLen:    2,
			wantSI:     []int{0, 1},
			wantLabels: []string{"photos", "videos"},
		},
		{
			name:       "tape 1 gets archive 2",
			tapeIndex:  1,
			wantLen:    1,
			wantSI:     []int{2},
			wantLabels: []string{"plex-group-snap"},
		},
		{
			name:      "out-of-range tape index errors",
			tapeIndex: 99,
			wantLen:   0,
			wantErr:   require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			wantErr := test.wantErr
			if wantErr == nil {
				wantErr = require.NoError
			}

			got, err := archivesForTape(state, test.tapeIndex)
			wantErr(t, err)
			require.Len(t, got, test.wantLen)

			for i, si := range test.wantSI {
				assert.Equal(t, si, got[i].SourceIndex)
				assert.Equal(t, test.wantLabels[i], got[i].Label, "label must be threaded from the resolved work list")
				assert.NotEmpty(t, got[i].Slices)
				assert.NotEmpty(t, got[i].PAR2Files)
			}
		})
	}
}

func TestArchivesForTapeMissingStaged(t *testing.T) {
	t.Parallel()

	state := &runState{
		plan: TapePlan{
			Copies: 1,
			Tapes: []PlannedTape{
				{Archives: []PlannedArchive{
					{SourceIndex: 0},
					{SourceIndex: 1},
				}},
			},
		},
		staged: []StagedArchive{
			// SourceIndex 1 is missing its staged slices.
			{SourceIndex: 0, Slices: []StagedSlice{{Path: "/staging/a0.000"}}},
		},
		par2: []PAR2Set{
			{SourceIndex: 0, Files: []StagedSlice{{Path: "/staging/a0.par2"}}},
			{SourceIndex: 1, Files: []StagedSlice{{Path: "/staging/a1.par2"}}},
		},
	}

	// A planned archive with no staged slices must fail loudly rather than be
	// omitted: writing manifest.json (the completeness signal) for a tape that
	// is missing a planned archive would be undetectable data loss.
	got, err := archivesForTape(state, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source index 1")
	assert.Nil(t, got)
}

func TestArchivesForTapeMissingPAR2(t *testing.T) {
	t.Parallel()

	state := &runState{
		plan: TapePlan{
			Copies: 1,
			Tapes: []PlannedTape{
				{Archives: []PlannedArchive{{SourceIndex: 0}}},
			},
		},
		staged: []StagedArchive{
			{SourceIndex: 0, Slices: []StagedSlice{{Path: "/staging/a0.000"}}},
		},
		// No PAR2 set for source index 0.
		par2: nil,
	}

	got, err := archivesForTape(state, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PAR2")
	assert.Nil(t, got)
}
