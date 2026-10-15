// Package recoverykit builds the optical recovery kit (SPEC §10): a
// self-contained ISO 9660 image that, with the tapes alone, lets a future
// operator recover the archives.
//
// Recovery binaries must be statically linked: decades from now there may be no
// package manager to resolve shared libraries, so Build fails rather than ship a
// useless disc.
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

// recoveryProcedureDoc is docs/recovery-procedure.md, shipped verbatim on the
// disc (a test asserts the copy matches; issue #21).
//
//go:generate cp ../../docs/recovery-procedure.md recovery-procedure.md
//go:embed recovery-procedure.md
var recoveryProcedureDoc []byte

// volumeIdentifier must use only d-characters and fit in 32 characters.
const volumeIdentifier = "TAPE_ARCHIVER_RECOVERY"

// On-disc layout (SPEC §10). These names survive ISO 9660 level-2 naming
// unchanged; names Build cannot control are keyed via readbackPath.
const (
	reportPath    = "report.pdf"
	manifestPath  = "manifest.sha256"
	procedurePath = "recovery-procedure.md"
	indexDir      = "ltfs-index"
	indexSuffix   = ".schema"
	binDir        = "bin"
	srcDir        = "src"
)

// These mirror the name-mangling rules of github.com/kdomanski/iso9660 v0.4.0
// and are PINNED to it: a dependency bump must re-verify them (the round-trip
// tests are the guard).
const (
	// ECMA-119 7.5 / 7.6.3 level-2 identifier caps.
	isoFileIdentifierMaxLength = 30
	isoDirIdentifierMaxLength  = 31
	// Extension component cap.
	isoExtensionMaxLength = 8
	// The ";1" version counts against the budget but is stripped on read-back.
	isoVersionLength = 1 // len("1")

	d1Characters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_!\"%&'()*+,-./:;<=>?"
)

// Input is the complete set of artifacts assembled into the recovery ISO.
type Input struct {
	// Report is the PDF run report, treated as opaque bytes.
	Report []byte
	// Manifest is the full SHA-256 manifest covering every on-tape file.
	Manifest []byte
	// TapeIndexes holds one captured LTFS index per physical tape.
	TapeIndexes []TapeIndex
	// BinariesDir holds the recovery binaries; each top-level file must be a
	// statically linked executable.
	BinariesDir string
	// SourcesDir holds the recovery tools' source archives (SPEC §2, §10). It must
	// yield at least one file so the tools can be rebuilt where the binaries no
	// longer run.
	SourcesDir string
}

// Manifest maps each disc path, exactly as the burned disc presents it on
// read-back (see readbackPath), to its lowercase hex SHA-256.
type Manifest map[string]string

// Bytes renders the manifest in sha256sum format, sorted by path.
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

func (m Manifest) add(discPath string, data []byte) {
	sum := sha256.Sum256(data)
	m[readbackPath(discPath)] = hex.EncodeToString(sum[:])
}

// readbackPath maps a staged disc path to the name the read-back presents. With
// no Rock Ridge, the writer applies the full ISO 9660 level-2 transform, not
// just lowercasing; modelling only lowercasing mis-keyed barcodes and let two
// barcodes collide to one index file (issue #153).
func readbackPath(discPath string) string {
	segments := strings.Split(discPath, "/")

	for i, segment := range segments {
		if i == len(segments)-1 {
			segments[i] = mangleReadbackFileName(segment)
		} else {
			segments[i] = mangleD1(segment, isoDirIdentifierMaxLength)
		}
	}

	return path.Join(segments...)
}

// mangleReadbackFileName reproduces the writer's mangleFileName, minus the ";1"
// version suffix the reader strips.
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

// mangleD1 reproduces the writer's mangleD1String, byte-wise as the writer does.
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

// TapeIndex is one tape's LTFS index backup, named by the tape's barcode.
type TapeIndex struct {
	// Barcode names the index file on the disc (ltfs-index/`barcode`.schema).
	Barcode string
	// Index is the LTFS index XML.
	Index []byte
}

// Build writes the ISO 9660 image for in to w and returns the disc-content
// Manifest the Burn phase verifies the burned disc against.
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
			slog.WarnContext(ctx, "recoverykit: cleaning up ISO staging directory", "error", cerr)
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

	// Distinct barcodes can mangle to one on-disc name, so key the collision
	// check on that name, not the barcode (issue #153).
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

// stageBinaries stages every top-level file in dir into /bin, failing on any
// non-static binary or an empty directory.
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

		slog.DebugContext(ctx, "recoverykit: staged recovery binary", "binary", entry.Name())

		staged++
	}

	if staged == 0 {
		return fmt.Errorf("no recovery binaries found in %s", dir)
	}

	slog.InfoContext(ctx, "recoverykit: staged recovery binaries", "count", staged, "source", dir)

	return nil
}

// stageSources stages every top-level file in dir into /src. An empty directory
// fails: a disc without source violates the 20-year recoverability principle.
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

		slog.DebugContext(ctx, "recoverykit: staged recovery source", "source", entry.Name())

		staged++
	}

	if staged == 0 {
		return fmt.Errorf("no recovery source archives found in %s", dir)
	}

	slog.InfoContext(ctx, "recoverykit: staged recovery sources", "count", staged, "source", dir)

	return nil
}

// assertStaticallyLinked rejects ELF files with a PT_INTERP header or DT_NEEDED
// entries; static and static-PIE binaries have neither.
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
