// Package recoverykit builds the optical recovery kit (SPEC §10): a
// self-contained ISO 9660 image that, together with the physical tapes, lets a
// future operator read, repair, decrypt, decompress, and unpack the archives
// with nothing but the disc and the tapes.
//
// Build assembles five kinds of artifact into one image:
//
//   - the PDF run report (consumed as opaque input bytes, so this package has no
//     compile-time dependency on pkg/report);
//   - the full SHA-256 manifest;
//   - a backup copy of each tape's LTFS index (from pkg/ltfs.ReadIndex), in case
//     the on-tape index is damaged;
//   - the static recovery binaries (age, par2, zstd, tar) staged from a
//     configurable source directory, plus the full step-by-step recovery
//     procedure (recovery-procedure.md), embedded from docs/recovery-procedure.md;
//   - the recovery tools' upstream source archives (SPEC §2, §10) staged from a
//     configurable directory, so the tools can be rebuilt from source on future
//     hardware the pinned static binaries cannot run on.
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

// On-disc layout (SPEC §10). The fixed artifact names below are short, lowercase,
// and free of interior dots beyond a single extension, so they survive ISO 9660
// level-2 naming (no Rock Ridge) unchanged. Names Build cannot control — the tape
// barcodes under indexDir and the source archive names under srcDir — are keyed in
// the manifest via readbackPath, which reproduces the writer's name transform so
// the manifest always matches what the burned disc presents on read-back.
const (
	reportPath    = "report.pdf"
	manifestPath  = "manifest.sha256"
	procedurePath = "recovery-procedure.md"
	indexDir      = "ltfs-index"
	indexSuffix   = ".schema"
	binDir        = "bin"
	srcDir        = "src"
)

// The following mirror the pinned github.com/kdomanski/iso9660 v0.4.0 writer's
// name-mangling rules (image_writer.go manglePath/mangleFileName/mangleD1String
// and iso9660.go's d1-character set), reproduced by readbackPath so the manifest
// keys match the names the read-back presents. They are PINNED to that version: a
// dependency bump must re-verify these against the writer (the round-trip tests,
// which read the real image back, are the guard).
const (
	// isoFileIdentifierMaxLength and isoDirIdentifierMaxLength are the ISO 9660
	// level-2 identifier length caps the writer enforces (ECMA-119 7.5 / 7.6.3).
	isoFileIdentifierMaxLength = 30
	isoDirIdentifierMaxLength  = 31
	// isoExtensionMaxLength is the writer's cap on the extension component.
	isoExtensionMaxLength = 8
	// isoVersionSuffixLength is len(";1") accounted for in the file-identifier
	// budget; the reader strips the ";1" version so it never appears on read-back.
	isoVersionLength = 1 // len("1")

	// d1Characters is the writer's d1-character set. Any byte outside it is folded
	// to '_'; the writer lowercases first, so the manifest keys are lowercase.
	d1Characters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_!\"%&'()*+,-./:;<=>?"
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
	// SourcesDir is the source directory holding the recovery tools' upstream
	// source archives (SPEC §2, §10 — "…plus their source"). Its top-level regular
	// files are staged verbatim into the disc's src/; unlike the binaries these are
	// archives, not executables, so they are not linkage-checked. It must be set and
	// yield at least one file — a disc without source cannot rebuild the tools on
	// hardware the pinned binaries do not run on. The archives are produced alongside
	// the binaries (nix/recovery-binaries.nix emits them under $out/src, the sibling
	// of $out/bin).
	SourcesDir string
}

// Manifest maps each disc-relative, slash-separated path Build stages to the
// lowercase hex SHA-256 of that file's content. It is the set of files a burned
// disc must contain, and their digests, for the Burn phase's read-back
// verification (SPEC §10; pkg/optical.Verify). Paths are recorded exactly as the
// burned disc presents them on read-back — this image carries no Rock Ridge, so
// the ISO 9660 mount transforms names (lowercasing, folding characters outside the
// d1 set to '_', joining interior dots, and truncating to the level-2 length caps;
// see readbackPath) — so a manifest built here compares equal to what
// pkg/optical.Verify walks off the mounted disc without any per-caller fix-up.
// Render it to standard sha256sum format with Bytes.
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
// burned disc presents on read-back (see readbackPath).
func (m Manifest) add(discPath string, data []byte) {
	sum := sha256.Sum256(data)
	m[readbackPath(discPath)] = hex.EncodeToString(sum[:])
}

// readbackPath maps a staged disc path to the path the read-back presents. The
// image carries no Rock Ridge, so the ISO 9660 mount (and the pure-Go reader)
// does not just lowercase names — it applies the full ISO 9660 level-2 transform
// the pinned writer performs: each path segment is lowercased, every byte outside
// the d1 set is folded to '_', interior dots in a file name are joined with '_'
// (the last dot separates the extension), and the stem, extension, and directory
// identifiers are truncated to the level-2 caps. The reader strips the ";1"
// version suffix, so it is not part of the read-back name. Modelling only the
// lowercasing (as an earlier version did) silently mis-keys any barcode carrying a
// space, an interior dot, a non-d1 character, or more characters than the on-disc
// budget allows, making post-burn verification fail on every attempt and letting
// two distinct barcodes collide to one on-disc index file (issue #153).
//
// This mirrors github.com/kdomanski/iso9660 v0.4.0 and is pinned to it; the
// round-trip tests read the real image back to prove the mapping stays exact.
func readbackPath(discPath string) string {
	segments := strings.Split(discPath, "/")

	for i, segment := range segments {
		if i == len(segments)-1 {
			// The final segment is a file name (mangleFileName), minus the version.
			segments[i] = mangleReadbackFileName(segment)
		} else {
			// Interior segments are directory identifiers (mangleDirectoryName).
			segments[i] = mangleD1(segment, isoDirIdentifierMaxLength)
		}
	}

	return path.Join(segments...)
}

// mangleReadbackFileName reproduces the writer's mangleFileName for v0.4.0 and
// returns the name the reader presents on read-back (i.e. without the ";1" version
// suffix the writer appends and the reader strips). It lowercases, splits off the
// final dot as the extension separator, joins any interior dots in the stem with
// '_', caps the extension at isoExtensionMaxLength, and caps the stem at whatever
// the 30-character file-identifier budget leaves after the version and extension.
func mangleReadbackFileName(input string) string {
	input = strings.ToLower(input)
	split := strings.Split(input, ".")

	var stem, extension string
	if len(split) == 1 {
		stem = split[0]
	} else {
		stem = strings.Join(split[:len(split)-1], "_")
		extension = split[len(split)-1]
	}

	extension = mangleD1(extension, isoExtensionMaxLength)

	maxStem := isoFileIdentifierMaxLength - (1 + isoVersionLength)
	if len(extension) > 0 {
		maxStem -= (1 + len(extension))
	}

	stem = mangleD1(stem, maxStem)

	if len(extension) > 0 {
		return stem + "." + extension
	}

	return stem
}

// mangleD1 reproduces the writer's mangleD1String for v0.4.0: it lowercases the
// input, then, byte by byte up to maxCharacters bytes, keeps bytes in the d1 set
// and folds every other byte to '_'. It operates on bytes, exactly as the writer
// does (there is no multibyte handling in a d1 identifier).
func mangleD1(input string, maxCharacters int) string {
	input = strings.ToLower(input)

	var builder strings.Builder

	for i := 0; i < len(input) && i < maxCharacters; i++ {
		if strings.IndexByte(d1Characters, input[i]) >= 0 {
			builder.WriteByte(input[i])
		} else {
			builder.WriteByte('_')
		}
	}

	return builder.String()
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
// linked ELF executable, if no source archive is present, or if the image cannot
// be written. ctx cancellation is honored between staging steps.
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

	if err := stageSources(ctx, writer, in.SourcesDir, manifest); err != nil {
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

	// The ISO 9660 writer mangles each barcode into the on-disc index file name
	// (readbackPath), and distinct barcodes can mangle to one name — e.g. a space or
	// a dot both fold toward '_'. Key the collision check on that real on-disc name,
	// not just the case-folded barcode, so no tape's index silently overwrites
	// another's (issue #153).
	seen := make(map[string]string, len(in.TapeIndexes))

	for _, tape := range in.TapeIndexes {
		if strings.TrimSpace(tape.Barcode) == "" {
			return fmt.Errorf("tape index has an empty barcode")
		}

		if len(tape.Index) == 0 {
			return fmt.Errorf("LTFS index for tape %s is empty", tape.Barcode)
		}

		key := readbackPath(path.Join(indexDir, tape.Barcode+indexSuffix))
		if prior, ok := seen[key]; ok {
			return fmt.Errorf("tape barcodes %q and %q collide to the same on-disc index file name %q", prior, tape.Barcode, key)
		}

		seen[key] = tape.Barcode
	}

	if in.BinariesDir == "" {
		return fmt.Errorf("binaries directory is required")
	}

	if in.SourcesDir == "" {
		return fmt.Errorf("sources directory is required")
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

// stageSources stages every top-level regular file in dir into /src, recording
// each staged file's read-back path and SHA-256 into manifest (SPEC §2, §10 — the
// recovery tools' source ships on the disc so the tools can be rebuilt on future
// hardware the pinned static binaries cannot run on). Unlike stageBinaries these
// are source archives, not executables, so there is no linkage check — but it
// otherwise fails on the same conditions: the directory cannot be read, a
// non-regular file is present, or the directory yields no source archives (a
// recovery kit that ships binaries but no source violates the 20-year
// recoverability principle — fail loudly, never silently drop the source).
func stageSources(ctx context.Context, writer *iso9660.ImageWriter, dir string, manifest Manifest) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read sources directory %s: %w", dir, err)
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
			return fmt.Errorf("stat recovery source %s: %w", origin, err)
		}

		if !info.Mode().IsRegular() {
			return fmt.Errorf("recovery source %s is not a regular file (mode %s); recovery sources must be plain archive files", origin, info.Mode())
		}

		target := path.Join(srcDir, entry.Name())
		if err := writer.AddLocalFile(origin, target); err != nil {
			return fmt.Errorf("stage recovery source %s: %w", origin, err)
		}

		digest, err := checksum.SHA256File(origin)
		if err != nil {
			return fmt.Errorf("checksum recovery source %s: %w", origin, err)
		}

		manifest[readbackPath(target)] = digest

		slog.Debug("recoverykit: staged recovery source", "source", entry.Name())

		staged++
	}

	if staged == 0 {
		return fmt.Errorf("no recovery source archives found in %s", dir)
	}

	slog.Info("recoverykit: staged recovery sources", "count", staged, "source", dir)

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
