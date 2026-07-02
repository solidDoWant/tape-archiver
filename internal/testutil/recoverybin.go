package testutil

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// recoveryBinaries are the recovery tools recoverykit stages into the ISO's /bin.
var recoveryBinaries = []string{"age", "par2", "zstd", "tar"}

// StaticELF returns a minimal, debug/elf-parseable ELF64 executable with only a
// PT_LOAD segment — no program interpreter and no shared-library dependencies,
// i.e. statically linked. It is the smallest fixture that satisfies
// recoverykit.Build's static-linkage check without needing a compiler, so tests
// that build a recovery ISO need no real static binaries.
func StaticELF() []byte {
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
	write64(0x400000)  // e_entry
	write64(ehSize)    // e_phoff
	write64(0)         // e_shoff
	write32(0)         // e_flags
	write16(ehSize)    // e_ehsize
	write16(phEntSize) // e_phentsize
	write16(1)         // e_phnum (one PT_LOAD)
	write16(0)         // e_shentsize
	write16(0)         // e_shnum
	write16(0)         // e_shstrndx

	write32(uint32(elf.PT_LOAD)) // p_type
	write32(0x4)                 // p_flags (R)
	write64(0)                   // p_offset
	write64(0x400000)            // p_vaddr
	write64(0x400000)            // p_paddr
	write64(0)                   // p_filesz
	write64(0)                   // p_memsz
	write64(0x1000)              // p_align

	return buf.Bytes()
}

// RecoveryBinariesDir creates a temp directory populated with the static recovery
// binaries (age, par2, zstd, tar) as minimal static ELF fixtures and returns its
// path. It satisfies recoverykit.Build's static-linkage requirement in tests
// (report/ISO building) without shipping real binaries.
func RecoveryBinariesDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	for _, name := range recoveryBinaries {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), StaticELF(), 0o755))
	}

	return dir
}
