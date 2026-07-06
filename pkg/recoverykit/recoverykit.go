// Package recoverykit builds the optical recovery kit (SPEC §10): a
// self-contained ISO 9660 image that, together with the physical tapes, lets a
// future operator read, repair, decrypt, decompress, and unpack the archives
// with nothing but the disc and the tapes.
//
// Build assembles four kinds of artifact into one image:
//
//   - the PDF run report (consumed as opaque input bytes, so this package has no
//     compile-time dependency on pkg/report);
//   - the full SHA-256 manifest;
//   - a backup copy of each tape's LTFS index (from pkg/ltfs.ReadIndex), in case
//     the on-tape index is damaged;
//   - the static recovery binaries (age, par2, zstd, tar) staged from a
//     configurable source directory, plus the full step-by-step recovery
//     procedure (recovery-procedure.md), embedded from docs/recovery-procedure.md.
//
// Recovery binaries MUST be statically linked. At restore time — potentially
// decades later, on unknown hardware with no package manager — a dynamically
// linked binary whose shared-library dependencies cannot be resolved is dead
// weight. Build therefore inspects every staged binary and fails the run if any
// is not statically linked, so a misconfigured run can never silently produce a
// useless recovery disc (SPEC §2: 20-year recoverability; everything is tested).
//
// The image is written with the pure-Go github.com/kdomanski/iso9660 writer so
// the build and its tests stay hermetic, with no runtime CLI dependency —
// consistent with pkg/report's pure-Go choice. This package emits an uncompressed
// image; compression of the .iso for delivery (SPEC §11) is the Report phase's
// concern, not this package's.
package recoverykit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kdomanski/iso9660"

	"github.com/solidDoWant/tape-archiver/pkg/checksum"
)

// recoveryProcedureDoc is the full operator recovery procedure, embedded so the
// disc always carries the same bytes as docs/recovery-procedure.md (a test
// asserts they are identical). It is shipped verbatim on the disc as
// recovery-procedure.md and is the authoritative, self-contained recovery
// reference — the PDF report carries only a concise copy (SPEC §10, issue #21).
//
//go:generate cp ../../docs/recovery-procedure.md recovery-procedure.md
//go:embed recovery-procedure.md
var recoveryProcedureDoc []byte

// volumeIdentifier is the ISO 9660 primary volume identifier. It uses only
// valid d-characters and is within the 32-character limit.
const volumeIdentifier = "TAPE_ARCHIVER_RECOVERY"

// On-disc layout (SPEC §10). Names are kept short and lowercase so they survive
// ISO 9660 level-2 naming (no Rock Ridge; identifiers are capped at 30
// characters) unchanged.
const (
	reportPath    = "report.pdf"
	manifestPath  = "manifest.sha256"
	procedurePath = "recovery-procedure.md"
	indexDir      = "ltfs-index"
	indexSuffix   = ".schema"
	binDir        = "bin"
)

// Input is the complete set of artifacts assembled into the recovery ISO. The
// report and manifest are supplied as bytes (input artifacts), so building and
// testing this package needs no live PDF or checksum tooling.
type Input struct {
	// Report is the PDF run report (SPEC §9), treated as opaque bytes.
	Report []byte
	// Manifest is the full SHA-256 manifest covering every on-tape file.
	Manifest []byte
	// TapeIndexes holds one captured LTFS index per physical tape.
	TapeIndexes []TapeIndex
	// BinariesDir is the source directory holding the static recovery binaries
	// (age, par2, zstd, tar). Its top-level regular files are staged verbatim;
	// each must be a statically linked native executable. The binaries are
	// produced and pinned elsewhere (the OCI image) to match the recovery-disc
	// versions — this package only stages what is present and proves it is
	// static.
	BinariesDir string
}

// Manifest maps each disc-relative, slash-separated path Build stages to the
// lowercase hex SHA-256 of that file's content. It is the set of files a burned
// disc must contain, and their digests, for the Burn phase's read-back
// verification (SPEC §10; pkg/optical.Verify). Paths are recorded exactly as the
// burned disc presents them on read-back — this image carries no Rock Ridge, so
// the ISO 9660 mount lowercases names — so a manifest built here compares equal
// to what pkg/optical.Verify walks off the mounted disc without any per-caller
// case fix-up. Render it to standard sha256sum format with Bytes.
type Manifest map[string]string

// Bytes renders the manifest to the standard sha256sum format — one
// "<hex-digest>  <path>" line per file, sorted by path for a deterministic,
// diff-stable file — which pkg/optical.ParseManifest reads back verbatim.
func (m Manifest) Bytes() []byte {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}

	sort.Strings(names)

	var builder strings.Builder
	for _, name := range names {
		fmt.Fprintf(&builder, "%s  %s\n", m[name], name)
	}

	return []byte(builder.String())
}

// add records the disc-relative path -> SHA-256 of data, under the path the
// burned disc presents on read-back (lowercased; see Manifest).
func (m Manifest) add(discPath string, data []byte) {
	sum := sha256.Sum256(data)
	m[readbackPath(discPath)] = hex.EncodeToString(sum[:])
}

// readbackPath maps a staged disc path to the path the read-back presents. The
// image carries no Rock Ridge, so the ISO 9660 mount (and the pure-Go reader)
// lowercases every name; the manifest keys must match that to compare equal.
func readbackPath(discPath string) string {
	return strings.ToLower(discPath)
}

// TapeIndex is one tape's LTFS index backup, named by the tape's barcode — the
// canonical physical ID (SPEC §6).
type TapeIndex struct {
	// Barcode is the library-read barcode / LTFS volume name. It names the
	// index file on the disc (ltfs-index/<barcode>.schema).
	Barcode string
	// Index is the LTFS index XML, as returned by pkg/ltfs.ReadIndex.
	Index []byte
}

// Build assembles in into a valid ISO 9660 image and writes it to w, returning
// the disc-content Manifest (every staged file's read-back path -> SHA-256) so
// the Burn phase can verify the burned disc against it (SPEC §10). It returns an
// error if any input is missing, if any recovery binary is not a statically
// linked ELF executable, or if the image cannot be written. ctx cancellation is
// honored between staging steps.
func Build(ctx context.Context, in Input, w io.Writer) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := in.validate(); err != nil {
		return nil, fmt.Errorf("recoverykit: %w", err)
	}

	writer, err := iso9660.NewWriter()
	if err != nil {
		return nil, fmt.Errorf("recoverykit: create ISO writer: %w", err)
	}
	defer func() {
		if cerr := writer.Cleanup(); cerr != nil {
			slog.Warn("recoverykit: cleaning up ISO staging directory", "error", cerr)
		}
	}()

	manifest := make(Manifest)

	for _, artifact := range []struct {
		name string
		data []byte
	}{
		{reportPath, in.Report},
		{manifestPath, in.Manifest},
		{procedurePath, recoveryProcedureDoc},
	} {
		if err := writer.AddFile(bytes.NewReader(artifact.data), artifact.name); err != nil {
			return nil, fmt.Errorf("recoverykit: stage %s: %w", artifact.name, err)
		}

		manifest.add(artifact.name, artifact.data)
	}

	for _, tape := range in.TapeIndexes {
		target := path.Join(indexDir, tape.Barcode+indexSuffix)
		if err := writer.AddFile(bytes.NewReader(tape.Index), target); err != nil {
			return nil, fmt.Errorf("recoverykit: stage LTFS index for tape %s: %w", tape.Barcode, err)
		}

		manifest.add(target, tape.Index)
	}

	if err := stageBinaries(ctx, writer, in.BinariesDir, manifest); err != nil {
		return nil, fmt.Errorf("recoverykit: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := writer.WriteTo(w, volumeIdentifier); err != nil {
		return nil, fmt.Errorf("recoverykit: write ISO: %w", err)
	}

	return manifest, nil
}

// validate checks that every required artifact is present and that the tape
// indexes are usable and uniquely named.
func (in Input) validate() error {
	if len(in.Report) == 0 {
		return fmt.Errorf("report PDF is empty")
	}

	if len(in.Manifest) == 0 {
		return fmt.Errorf("SHA-256 manifest is empty")
	}

	if len(in.TapeIndexes) == 0 {
		return fmt.Errorf("at least one tape LTFS index is required")
	}

	// On-disc index names are case-folded; reject collisions so no tape's index
	// silently overwrites another's.
	seen := make(map[string]string, len(in.TapeIndexes))

	for _, tape := range in.TapeIndexes {
		if strings.TrimSpace(tape.Barcode) == "" {
			return fmt.Errorf("tape index has an empty barcode")
		}

		if len(tape.Index) == 0 {
			return fmt.Errorf("LTFS index for tape %s is empty", tape.Barcode)
		}

		key := strings.ToLower(tape.Barcode)
		if prior, ok := seen[key]; ok {
			return fmt.Errorf("tape barcodes %q and %q collide to the same index file name", prior, tape.Barcode)
		}

		seen[key] = tape.Barcode
	}

	if in.BinariesDir == "" {
		return fmt.Errorf("binaries directory is required")
	}

	return nil
}

// stageBinaries stages every top-level regular file in dir into /bin, after
// proving each is a statically linked ELF executable, recording each staged
// binary's read-back path and SHA-256 into manifest. It fails if the directory
// cannot be read, if a non-regular file is present, if a binary is not static,
// or if the directory yields no binaries (a recovery kit with no tooling is
// useless).
func stageBinaries(ctx context.Context, writer *iso9660.ImageWriter, dir string, manifest Manifest) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read binaries directory %s: %w", dir, err)
	}

	staged := 0

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		if entry.IsDir() {
			continue
		}

		origin := filepath.Join(dir, entry.Name())

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat recovery binary %s: %w", origin, err)
		}

		if !info.Mode().IsRegular() {
			return fmt.Errorf("recovery binary %s is not a regular file (mode %s); recovery binaries must be statically linked native executables", origin, info.Mode())
		}

		if err := assertStaticallyLinked(origin); err != nil {
			return err
		}

		target := path.Join(binDir, entry.Name())
		if err := writer.AddLocalFile(origin, target); err != nil {
			return fmt.Errorf("stage recovery binary %s: %w", origin, err)
		}

		digest, err := checksum.SHA256File(origin)
		if err != nil {
			return fmt.Errorf("checksum recovery binary %s: %w", origin, err)
		}

		manifest[readbackPath(target)] = digest

		slog.Debug("recoverykit: staged recovery binary", "binary", entry.Name())

		staged++
	}

	if staged == 0 {
		return fmt.Errorf("no recovery binaries found in %s", dir)
	}

	slog.Info("recoverykit: staged recovery binaries", "count", staged, "source", dir)

	return nil
}

// assertStaticallyLinked returns an error unless pathName is a statically linked
// ELF executable. A dynamically linked binary carries a PT_INTERP program header
// (naming the dynamic loader) and/or DT_NEEDED shared-library dependencies;
// statically linked and static-PIE binaries have neither. Detection uses the
// standard library's debug/elf so it needs no external tool and stays hermetic.
func assertStaticallyLinked(pathName string) error {
	file, err := elf.Open(pathName)
	if err != nil {
		return fmt.Errorf("recovery binary %s is not a valid ELF executable (recovery binaries must be statically linked native executables): %w", pathName, err)
	}
	defer func() { _ = file.Close() }()

	for _, prog := range file.Progs {
		if prog.Type == elf.PT_INTERP {
			return fmt.Errorf("recovery binary %s is dynamically linked (it declares a program interpreter); it must be statically linked so it runs on bare recovery hardware with no shared libraries", pathName)
		}
	}

	libraries, err := file.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("inspecting dynamic dependencies of recovery binary %s: %w", pathName, err)
	}

	if len(libraries) > 0 {
		return fmt.Errorf("recovery binary %s is dynamically linked (it needs shared libraries %v); it must be statically linked", pathName, libraries)
	}

	return nil
}
