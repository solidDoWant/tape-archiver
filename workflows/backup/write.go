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

// The on-tape directory layout (SPEC §6):
//
//   archives/NNN/<files>   per-archive slices and PAR2 recovery files,
//                          where NNN is the zero-padded source index.
//   manifest.json          top-level checksum manifest, written LAST so its
//                          presence signals a complete write.
//
// Writing the manifest last means an incomplete write can always be identified:
// a tape without manifest.json was not fully written by this run.

const (
	// archiveDirFmt is the per-archive directory name under the LTFS root.
	archiveDirFmt = "archives/%03d"
	// manifestName is the top-level manifest filename at the LTFS root.
	manifestName = "manifest.json"
)

// TapeWriteArchive holds the staged files for one archive to be copied to tape.
// It is embedded in WriteTreeInput so the WriteTree activity knows what to copy.
type TapeWriteArchive struct {
	// SourceIndex identifies the archive within the run (its position in
	// Config.Sources and the matching StagedArchive/PAR2Set).
	SourceIndex int
	// Slices are the staged, checksummed archive slice files in order.
	Slices []StagedSlice
	// PAR2Files are the staged PAR2 recovery files for this archive.
	PAR2Files []StagedSlice
}

// TapeManifest is the per-tape checksum manifest written last to the LTFS root.
// It records the tape's identity, provenance, and the SHA-256 of every file on
// the tape so a future recoverer can verify the tape's contents without re-reading
// every byte (SPEC §6).
type TapeManifest struct {
	// Barcode is the tape's library barcode and LTFS volume name (SPEC §6).
	Barcode tape.Barcode `json:"barcode"`
	// TapeIndex is the 0-based index of this logical tape in the plan.
	TapeIndex int `json:"tape_index"`
	// CopyIndex is the 0-based copy number of this physical tape.
	CopyIndex int `json:"copy_index"`
	// Archives lists the per-archive file inventory in source order.
	Archives []ArchiveManifest `json:"archives"`
}

// ArchiveManifest is the per-archive section of the tape manifest.
type ArchiveManifest struct {
	// SourceIndex ties the entry back to its Config.Source and StagedArchive.
	SourceIndex int `json:"source_index"`
	// Files lists the archive slice files on tape, in slice order.
	Files []ManifestFile `json:"files"`
	// PAR2Files lists the PAR2 recovery files on tape.
	PAR2Files []ManifestFile `json:"par2_files"`
}

// ManifestFile records one file on tape with its tape-relative path and SHA-256
// checksum. The SHA-256 is the precomputed digest from Prepare/GeneratePAR2 —
// no computation occurs in the write window (SPEC §14, CLAUDE.md).
type ManifestFile struct {
	// TapePath is the path relative to the LTFS root (e.g. archives/000/archive.000).
	TapePath string `json:"tape_path"`
	// SHA256 is the lowercase hex SHA-256 digest of the file's contents.
	SHA256 string `json:"sha256"`
	// SizeBytes is the file's size on disk (= on tape, since LTFS stores files
	// verbatim).
	SizeBytes int64 `json:"size_bytes"`
}

// copyTape copies the staged archive slices and PAR2 recovery files for each
// archive to the LTFS mountpoint. Each archive lands under archives/NNN/
// (NNN = zero-padded source index). The context is checked between files so a
// long copy honours cancellation without leaving a partial file in the
// destination.
//
// This is a pure disk→tape copy — no checksumming or computation occurs here.
// The precomputed SHA-256s from Prepare/GeneratePAR2 populate the manifest
// written by writeManifest.
func copyTape(ctx context.Context, mountpoint string, archives []TapeWriteArchive) error {
	for _, archive := range archives {
		dir := filepath.Join(mountpoint, fmt.Sprintf(archiveDirFmt, archive.SourceIndex))

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

// copyFile copies src to dst, creating dst if it does not exist. It returns an
// error if src cannot be opened or if the copy fails. The dst is created with
// 0o644 permissions.
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

// buildManifest constructs a TapeManifest from the WriteTreeInput, mapping each
// archive's staged slice and PAR2 file to its on-tape path and precomputed SHA-256.
// It does not read any files — all data comes from the already-computed fields in
// the TapeWriteArchive slices (SPEC §14: no computation in the write window).
func buildManifest(barcode tape.Barcode, tapeIndex, copyIndex int, archives []TapeWriteArchive) TapeManifest {
	archiveManifests := make([]ArchiveManifest, 0, len(archives))

	for _, archive := range archives {
		dir := fmt.Sprintf(archiveDirFmt, archive.SourceIndex)

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

// writeManifest serialises manifest as JSON and writes it to the top-level
// manifest.json at the LTFS mountpoint. Writing the manifest last ensures that
// any tape without manifest.json was not fully written — a recoverer can use this
// as a completeness signal (SPEC §6).
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

// archivesForTape builds the TapeWriteArchive list for the given logical tape
// index, pulling archive slices and PAR2 files from the run state. The Verify
// phase guarantees all referenced archives are present; a missing staged slice
// or PAR2 set is therefore a programming/state error and is reported as one
// rather than silently omitted — writing a tape that is missing a planned
// archive but still carries a "complete" manifest.json would be undetectable
// data loss (project principle 3, SPEC §2).
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
			Slices:      staged.Slices,
			PAR2Files:   par2.Files,
		})
	}

	return archives, nil
}
