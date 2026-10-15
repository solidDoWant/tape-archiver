package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// On-tape layout (SPEC §6): archives/NNN-<label>/<slices and PAR2 files>, plus
// manifest.json at the root, written LAST so a tape without it is known to be
// incompletely written.

const (
	manifestName = "manifest.json"
)

// TapeWriteArchive holds the staged files for one archive to be copied to tape.
type TapeWriteArchive struct {
	// SourceIndex is the NNN directory prefix and the archive's unique key.
	SourceIndex int
	// Label is the sanitized directory name suffix; it may be empty or shared.
	Label     string
	Slices    []StagedSlice
	PAR2Files []StagedSlice
}

// TapeManifest is the per-tape checksum manifest written last to the LTFS root
// (SPEC §6).
type TapeManifest struct {
	Barcode   tape.Barcode      `json:"barcode"`
	TapeIndex int               `json:"tape_index"`
	CopyIndex int               `json:"copy_index"`
	Archives  []ArchiveManifest `json:"archives"`
}

// ArchiveManifest is the per-archive section of the tape manifest.
type ArchiveManifest struct {
	SourceIndex int            `json:"source_index"`
	Files       []ManifestFile `json:"files"`
	PAR2Files   []ManifestFile `json:"par2_files"`
}

// ManifestFile records one file on tape. SHA256 is the digest precomputed
// before the write window; nothing is computed while writing (SPEC §14).
type ManifestFile struct {
	// TapePath is relative to the LTFS root.
	TapePath  string `json:"tape_path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// copyTape copies each archive's slices and PAR2 files to the LTFS mountpoint.
// It is a pure disk-to-tape copy: no checksumming happens in the write window.
func copyTape(ctx context.Context, mountpoint string, archives []TapeWriteArchive) error {
	for _, archive := range archives {
		dir := filepath.Join(mountpoint, archiveDirName(archive.SourceIndex, archive.Label))

		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create archive directory %s: %w", dir, err)
		}

		all := make([]StagedSlice, 0, len(archive.Slices)+len(archive.PAR2Files))
		all = append(all, archive.Slices...)
		all = append(all, archive.PAR2Files...)

		for _, file := range all {
			if err := ctx.Err(); err != nil {
				return err
			}

			dst := filepath.Join(dir, filepath.Base(file.Path))

			if err := copyFile(ctx, file.Path, dst); err != nil {
				return fmt.Errorf("copy %s to tape: %w", filepath.Base(file.Path), err)
			}
		}
	}

	return nil
}

func copyFile(_ context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}

	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create destination %s: %w", dst, err)
	}

	defer func() {
		_ = out.Close()
	}()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy data: %w", err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("close destination %s: %w", dst, err)
	}

	return nil
}

// buildManifest maps the staged files to their on-tape paths and precomputed
// SHA-256s without reading any file (SPEC §14).
func buildManifest(barcode tape.Barcode, tapeIndex, copyIndex int, archives []TapeWriteArchive) TapeManifest {
	archiveManifests := make([]ArchiveManifest, 0, len(archives))

	for _, archive := range archives {
		dir := archiveDirName(archive.SourceIndex, archive.Label)

		files := make([]ManifestFile, 0, len(archive.Slices))
		for _, slice := range archive.Slices {
			files = append(files, ManifestFile{
				TapePath:  filepath.Join(dir, filepath.Base(slice.Path)),
				SHA256:    slice.SHA256,
				SizeBytes: slice.SizeBytes,
			})
		}

		par2Files := make([]ManifestFile, 0, len(archive.PAR2Files))
		for _, f := range archive.PAR2Files {
			par2Files = append(par2Files, ManifestFile{
				TapePath:  filepath.Join(dir, filepath.Base(f.Path)),
				SHA256:    f.SHA256,
				SizeBytes: f.SizeBytes,
			})
		}

		archiveManifests = append(archiveManifests, ArchiveManifest{
			SourceIndex: archive.SourceIndex,
			Files:       files,
			PAR2Files:   par2Files,
		})
	}

	return TapeManifest{
		Barcode:   barcode,
		TapeIndex: tapeIndex,
		CopyIndex: copyIndex,
		Archives:  archiveManifests,
	}
}

// writeManifest writes manifest.json to the LTFS root.
func writeManifest(mountpoint string, manifest TapeManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	path := filepath.Join(mountpoint, manifestName)

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write manifest to %s: %w", path, err)
	}

	return nil
}

// archivesForTape builds the write list for one logical tape. A missing staged
// slice or PAR2 set is an error, never an omission: a tape lacking a planned
// archive but carrying a manifest.json would be undetectable data loss.
func archivesForTape(state *runState, tapeIndex int) ([]TapeWriteArchive, error) {
	if tapeIndex >= len(state.plan.Tapes) {
		return nil, fmt.Errorf("tape index %d out of range (plan has %d tapes)", tapeIndex, len(state.plan.Tapes))
	}

	planned := state.plan.Tapes[tapeIndex]

	slicesByIndex := make(map[int]StagedArchive, len(state.staged))
	for _, a := range state.staged {
		slicesByIndex[a.SourceIndex] = a
	}

	par2ByIndex := make(map[int]PAR2Set, len(state.par2))
	for _, p := range state.par2 {
		par2ByIndex[p.SourceIndex] = p
	}

	labelByIndex := make(map[int]string, len(state.resolved))
	for _, r := range state.resolved {
		labelByIndex[r.SourceIndex] = r.Label
	}

	archives := make([]TapeWriteArchive, 0, len(planned.Archives))

	for _, placement := range planned.Archives {
		staged, ok := slicesByIndex[placement.SourceIndex]
		if !ok {
			return nil, fmt.Errorf("tape %d: planned archive (source index %d) has no staged slices",
				tapeIndex, placement.SourceIndex)
		}

		par2, ok := par2ByIndex[placement.SourceIndex]
		if !ok {
			return nil, fmt.Errorf("tape %d: planned archive (source index %d) has no PAR2 recovery set",
				tapeIndex, placement.SourceIndex)
		}

		archives = append(archives, TapeWriteArchive{
			SourceIndex: placement.SourceIndex,
			Label:       labelByIndex[placement.SourceIndex],
			Slices:      staged.Slices,
			PAR2Files:   par2.Files,
		})
	}

	return archives, nil
}
