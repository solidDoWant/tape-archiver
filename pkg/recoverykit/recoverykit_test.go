package recoverykit_test

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kdomanski/iso9660"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/recoverykit"
)

// sourceArchives mirror nix/recovery-binaries.nix's $out/src naming.
var sourceArchives = map[string][]byte{
	"age-1.3.1.tar.gz":  []byte("fixture age source\n"),
	"par2-1.0.0.tar.gz": []byte("fixture par2 source\n"),
	"zstd-1.5.6.tar.gz": []byte("fixture zstd source\n"),
	"tar-1.35.tar.gz":   []byte("fixture tar source\n"),
}

func sourcesDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for name, content := range sourceArchives {
		writeFile(t, filepath.Join(dir, name), content)
	}

	return dir
}

func completeInput(t *testing.T) recoverykit.Input {
	t.Helper()

	binDir := t.TempDir()
	for _, name := range []string{"age", "par2", "zstd", "tar"} {
		writeFile(t, filepath.Join(binDir, name), staticELF())
	}

	return recoverykit.Input{
		Report:   []byte("%PDF-1.7\nfixture report\n%%EOF\n"),
		Manifest: []byte("9f86d081884c7d659a2feaa0c55ad015  photos.tar.zst.age.000\n"),
		TapeIndexes: []recoverykit.TapeIndex{
			{Barcode: "TAPE0001L8", Index: []byte(`<ltfsindex><generationnumber>1</generationnumber></ltfsindex>`)},
			{Barcode: "TAPE0002L8", Index: []byte(`<ltfsindex><generationnumber>2</generationnumber></ltfsindex>`)},
		},
		BinariesDir: binDir,
		SourcesDir:  sourcesDir(t),
	}
}

func TestBuild_RoundTrip(t *testing.T) {
	t.Parallel()

	in := completeInput(t)

	var buf bytes.Buffer

	manifest, err := recoverykit.Build(t.Context(), in, &buf)
	require.NoError(t, err)

	files := readISO(t, buf.Bytes())

	procedureDoc, err := os.ReadFile("recovery-procedure.md")
	require.NoError(t, err)

	want := map[string][]byte{
		"report.pdf":                   in.Report,
		"manifest.sha256":              in.Manifest,
		"recovery-procedure.md":        procedureDoc,
		"ltfs-index/tape0001l8.schema": in.TapeIndexes[0].Index,
		"ltfs-index/tape0002l8.schema": in.TapeIndexes[1].Index,
		"bin/age":                      staticELF(),
		"bin/par2":                     staticELF(),
		"bin/zstd":                     staticELF(),
		"bin/tar":                      staticELF(),
	}

	for name, content := range want {
		got, ok := files[name]
		require.Truef(t, ok, "expected %s in ISO; got paths %v", name, keys(files))
		assert.Equalf(t, content, got, "content mismatch for %s", name)
	}

	// Source archive names are mangled by the writer, so match them by content.
	srcFiles := make(map[string][]byte)

	for name, content := range files {
		if strings.HasPrefix(name, "src/") {
			srcFiles[name] = content
		}
	}

	assert.Lenf(t, srcFiles, len(sourceArchives), "every source archive must be staged under src/: %v", keys(srcFiles))

	seenSources := make(map[string]bool)

	for name, content := range srcFiles {
		matched := ""

		for origin, want := range sourceArchives {
			if bytes.Equal(content, want) {
				matched = origin
				break
			}
		}

		require.NotEmptyf(t, matched, "src file %s does not match any fixture source archive", name)
		assert.Falsef(t, seenSources[matched], "source archive %s staged more than once", matched)
		seenSources[matched] = true

		sum := sha256.Sum256(content)
		assert.Equalf(t, hex.EncodeToString(sum[:]), manifest[name],
			"manifest digest mismatch for %s", name)
	}

	totalFiles := len(want) + len(sourceArchives)

	assert.Lenf(t, files, totalFiles, "unexpected extra files in ISO: %v", keys(files))

	assert.Lenf(t, manifest, totalFiles, "manifest must list exactly the on-disc files")

	for name, content := range want {
		sum := sha256.Sum256(content)
		assert.Equalf(t, hex.EncodeToString(sum[:]), manifest[name],
			"manifest digest mismatch for %s", name)
	}

	for name := range manifest {
		_, ok := files[name]
		assert.Truef(t, ok, "manifest path %q is not a read-back path of the ISO", name)
	}
}

func TestBuild_RejectsDynamicBinary(t *testing.T) {
	t.Parallel()

	in := completeInput(t)
	writeFile(t, filepath.Join(in.BinariesDir, "age"), dynamicELF())

	_, err := recoverykit.Build(t.Context(), in, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dynamically linked")
}

func TestBuild_RejectsNonELFBinary(t *testing.T) {
	t.Parallel()

	in := completeInput(t)
	writeFile(t, filepath.Join(in.BinariesDir, "age"), []byte("#!/bin/sh\necho not static\n"))

	_, err := recoverykit.Build(t.Context(), in, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid ELF executable")
}

func TestBuild_Validation(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mutate  func(*recoverykit.Input)
		wantErr string
	}{
		"empty report":      {func(in *recoverykit.Input) { in.Report = nil }, "report PDF is empty"},
		"empty manifest":    {func(in *recoverykit.Input) { in.Manifest = nil }, "SHA-256 manifest is empty"},
		"no tapes":          {func(in *recoverykit.Input) { in.TapeIndexes = nil }, "at least one tape"},
		"empty barcode":     {func(in *recoverykit.Input) { in.TapeIndexes[0].Barcode = "" }, "empty barcode"},
		"empty index":       {func(in *recoverykit.Input) { in.TapeIndexes[0].Index = nil }, "is empty"},
		"colliding barcode": {func(in *recoverykit.Input) { in.TapeIndexes[1].Barcode = "tape0001l8" }, "collide"},
		"no binaries dir":   {func(in *recoverykit.Input) { in.BinariesDir = "" }, "binaries directory is required"},
		"no sources dir":    {func(in *recoverykit.Input) { in.SourcesDir = "" }, "sources directory is required"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			in := completeInput(t)
			test.mutate(&in)

			_, err := recoverykit.Build(t.Context(), in, io.Discard)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}

func TestBuild_EmptyBinariesDirFails(t *testing.T) {
	t.Parallel()

	in := completeInput(t)
	in.BinariesDir = t.TempDir()

	_, err := recoverykit.Build(t.Context(), in, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no recovery binaries found")
}

// TestBuild_MangledBarcodeIndexesRoundTrip proves readbackPath against the real
// writer for barcodes the ISO 9660 writer mangles (issue #153).
func TestBuild_MangledBarcodeIndexesRoundTrip(t *testing.T) {
	t.Parallel()

	barcodes := map[string]string{
		"interior dot":       "TAPE.01",
		"embedded space":     "TAPE 01",
		"non-d1 punctuation": "TAPE#01",
		"over name budget":   "TAPE0001L8-VERY-LONG-BARCODE-1234567890",
		"standard label":     "TAPE0001L8",
	}

	for name, barcode := range barcodes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			index := []byte(`<ltfsindex><generationnumber>7</generationnumber></ltfsindex>`)

			in := completeInput(t)
			in.TapeIndexes = []recoverykit.TapeIndex{{Barcode: barcode, Index: index}}

			var buf bytes.Buffer

			manifest, err := recoverykit.Build(t.Context(), in, &buf)
			require.NoError(t, err)

			files := readISO(t, buf.Bytes())

			var indexKey string

			for key := range manifest {
				if strings.HasPrefix(key, "ltfs-index/") {
					require.Emptyf(t, indexKey, "expected one ltfs-index entry; found %q and %q", indexKey, key)
					indexKey = key
				}
			}

			require.NotEmptyf(t, indexKey, "manifest records no ltfs-index entry for barcode %q", barcode)

			got, ok := files[indexKey]
			require.Truef(t, ok, "LTFS index for barcode %q not found at manifest path %q; read-back paths: %v", barcode, indexKey, keys(files))
			assert.Equalf(t, index, got, "LTFS index content mismatch for barcode %q", barcode)

			sum := sha256.Sum256(index)
			assert.Equalf(t, hex.EncodeToString(sum[:]), manifest[indexKey], "manifest digest mismatch for barcode %q", barcode)
		})
	}
}

// TestBuild_RejectsMangledBarcodeCollision: '.' and ' ' both fold to '_', so
// these barcodes collide on disc and must fail the build.
func TestBuild_RejectsMangledBarcodeCollision(t *testing.T) {
	t.Parallel()

	in := completeInput(t)
	in.TapeIndexes = []recoverykit.TapeIndex{
		{Barcode: "TAPE.01", Index: []byte("<ltfsindex>1</ltfsindex>")},
		{Barcode: "TAPE 01", Index: []byte("<ltfsindex>2</ltfsindex>")},
	}

	var buf bytes.Buffer

	_, err := recoverykit.Build(t.Context(), in, &buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collide")
	assert.Contains(t, err.Error(), "TAPE.01")
	assert.Contains(t, err.Error(), "TAPE 01")
	assert.Zerof(t, buf.Len(), "no image must be written when two barcodes collide")
}

func TestBuild_EmptySourcesDirFails(t *testing.T) {
	t.Parallel()

	in := completeInput(t)
	in.SourcesDir = t.TempDir()

	_, err := recoverykit.Build(t.Context(), in, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no recovery source archives found")
}

// TestRecoveryProcedureDocMatchesCanonical guards against the embedded copy
// drifting from docs/recovery-procedure.md.
func TestRecoveryProcedureDocMatchesCanonical(t *testing.T) {
	t.Parallel()

	shipped, err := os.ReadFile("recovery-procedure.md")
	require.NoError(t, err)

	canonical, err := os.ReadFile(filepath.Join("..", "..", "docs", "recovery-procedure.md"))
	require.NoError(t, err)

	assert.Equalf(t, string(canonical), string(shipped),
		"pkg/recoverykit/recovery-procedure.md is out of sync with docs/recovery-procedure.md; "+
			"run `go generate ./pkg/recoverykit/` (or copy the doc) to resync")
}

func readISO(t *testing.T, image []byte) map[string][]byte {
	t.Helper()

	img, err := iso9660.OpenImage(bytes.NewReader(image))
	require.NoError(t, err)

	root, err := img.RootDir()
	require.NoError(t, err)

	files := make(map[string][]byte)

	var walk func(dir *iso9660.File, prefix string)

	walk = func(dir *iso9660.File, prefix string) {
		children, err := dir.GetChildren()
		require.NoError(t, err)

		for _, child := range children {
			full := path.Join(prefix, child.Name())

			if child.IsDir() {
				walk(child, full)
				continue
			}

			data, err := io.ReadAll(child.Reader())
			require.NoError(t, err)

			files[full] = data
		}
	}

	walk(root, "")

	return files
}

func writeFile(t *testing.T, name string, data []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(name, data, 0o755))
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

func staticELF() []byte { return minimalELF(elf.PT_LOAD) }

func dynamicELF() []byte { return minimalELF(elf.PT_INTERP, elf.PT_LOAD) }

// minimalELF builds a section-less ELF64 executable with the given program
// headers, so the linkage check is testable without a compiler.
func minimalELF(progTypes ...elf.ProgType) []byte {
	const (
		ehSize    = 64
		phEntSize = 56
	)

	var buf bytes.Buffer

	buf.Write([]byte{0x7f, 'E', 'L', 'F'})
	buf.WriteByte(byte(elf.ELFCLASS64))
	buf.WriteByte(byte(elf.ELFDATA2LSB))
	buf.WriteByte(byte(elf.EV_CURRENT))
	buf.WriteByte(byte(elf.ELFOSABI_NONE))
	buf.Write(make([]byte, 8)) // pad e_ident to 16 bytes

	le := binary.LittleEndian
	write16 := func(v uint16) { _ = binary.Write(&buf, le, v) }
	write32 := func(v uint32) { _ = binary.Write(&buf, le, v) }
	write64 := func(v uint64) { _ = binary.Write(&buf, le, v) }

	write16(uint16(elf.ET_EXEC))   // e_type
	write16(uint16(elf.EM_X86_64)) // e_machine
	write32(uint32(elf.EV_CURRENT))
	write64(0x400000)               // e_entry
	write64(ehSize)                 // e_phoff
	write64(0)                      // e_shoff
	write32(0)                      // e_flags
	write16(ehSize)                 // e_ehsize
	write16(phEntSize)              // e_phentsize
	write16(uint16(len(progTypes))) // e_phnum
	write16(0)                      // e_shentsize
	write16(0)                      // e_shnum
	write16(0)                      // e_shstrndx

	for _, progType := range progTypes {
		write32(uint32(progType)) // p_type
		write32(0x4)              // p_flags (R)
		write64(0)                // p_offset
		write64(0x400000)         // p_vaddr
		write64(0x400000)         // p_paddr
		write64(0)                // p_filesz
		write64(0)                // p_memsz
		write64(0x1000)           // p_align
	}

	return buf.Bytes()
}
