package backup_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/agewrap"
	"github.com/solidDoWant/tape-archiver/pkg/archive"
	"github.com/solidDoWant/tape-archiver/pkg/checksum"
	"github.com/solidDoWant/tape-archiver/pkg/par2"
)

// These tests exercise the failure scenarios in docs/recovery-procedure.md
// against real staged artifacts, recovering with the binaries the recovery disc
// ships. They skip when those binaries are not on PATH.

const (
	recoveryRedundancyPercent = 30
	recoverySliceSize         = 64 * 1024
)

// TestRecoveryChain_PAR2RepairsWithinCapacity is scenario (a): damage within PAR2
// redundancy is repaired to the exact original bytes.
func TestRecoveryChain_PAR2RepairsWithinCapacity(t *testing.T) {
	requireRecoveryTools(t)

	dir := t.TempDir()
	identityPath, recipient := generateAgeKeypair(t)
	slices, want := buildArchive(t, dir, recipient)

	// Corrupt a small run of bytes in one slice — well within 30% redundancy.
	corruptFile(t, slices[len(slices)/2], 128, 200)

	got := recoverArchive(t, dir, identityPath, true)
	assert.Equal(t, want, got, "PAR2 repair must reconstruct the exact bytes, so the archive recovers fully")
}

// TestRecoveryChain_PAR2FailsBeyondCapacity is scenario (b): damage beyond PAR2
// capacity cannot be repaired, signalling fallback to another copy.
func TestRecoveryChain_PAR2FailsBeyondCapacity(t *testing.T) {
	requireRecoveryTools(t)

	dir := t.TempDir()
	_, recipient := generateAgeKeypair(t)
	slices, _ := buildArchive(t, dir, recipient)

	require.GreaterOrEqual(t, len(slices), 3, "need several slices to exceed capacity")

	// Destroy more than half the slices — far beyond 30% redundancy.
	damaged := slices[:len(slices)/2+1]

	for _, slice := range damaged {
		require.NoError(t, os.WriteFile(slice, make([]byte, fileSize(t, slice)), 0o644))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "par2", "repair", "archive.par2")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.Errorf(t, err, "par2 repair must fail when damage exceeds capacity; output:\n%s", out)
}

// TestRecoveryChain_AgeAbortsOnTruncation is scenario (c): age authenticates each
// chunk, so a truncated stream fails rather than yielding truncated data.
func TestRecoveryChain_AgeAbortsOnTruncation(t *testing.T) {
	requireRecoveryTools(t)

	dir := t.TempDir()
	identityPath, recipient := generateAgeKeypair(t)
	slices, _ := buildArchive(t, dir, recipient)

	// Reassemble the age stream, then truncate it to half.
	stream := concatSlices(t, slices)
	truncated := filepath.Join(dir, "archive.age")
	require.NoError(t, os.WriteFile(truncated, stream[:len(stream)/2], 0o644))

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "age", "-d", "-i", identityPath, "-o", filepath.Join(dir, "out.zst"), truncated)
	out, err := cmd.CombinedOutput()
	require.Errorf(t, err, "age must reject a truncated stream; output:\n%s", out)
}

// TestRecoveryChain_ChecksumDetectsCorruption is scenario (d): the SHA-256
// manifest catches a byte flip.
func TestRecoveryChain_ChecksumDetectsCorruption(t *testing.T) {
	requireRecoveryTools(t)

	dir := t.TempDir()
	_, recipient := generateAgeKeypair(t)
	slices, _ := buildArchive(t, dir, recipient)

	slice := slices[0]
	digest, err := checksum.SHA256File(slice)
	require.NoError(t, err)

	require.NoError(t, checksum.Verify(slice, digest), "clean file must verify")

	corruptFile(t, slice, 10, 1)
	require.Error(t, checksum.Verify(slice, digest), "a byte flip must fail SHA-256 verification")
}

// buildArchive stages one archive through the Prepare pipeline plus PAR2 and
// returns the slice paths and original payload.
func buildArchive(t *testing.T, dir, recipient string) (slices []string, payload []byte) {
	t.Helper()

	// Incompressible, so zstd still yields several slices.
	payload = pseudoRandom(512 * 1024)

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "payload.bin"), payload, 0o644))

	var tarball bytes.Buffer
	require.NoError(t, archive.Tar(t.Context(), &tarball, src))

	var compressed bytes.Buffer
	require.NoError(t, archive.Compress(t.Context(), &compressed, &tarball))

	var encrypted bytes.Buffer
	require.NoError(t, agewrap.Encrypt(t.Context(), &encrypted, &compressed, recipient))

	slices, err := archive.Split(t.Context(), &encrypted, recoverySliceSize, dir, "archive")
	require.NoError(t, err)
	require.NotEmpty(t, slices)

	require.NoError(t, par2.Generate(t.Context(), filepath.Join(dir, "archive.par2"), slices, recoveryRedundancyPercent))

	return slices, payload
}

// recoverArchive runs the documented recovery and returns payload.bin's bytes.
func recoverArchive(t *testing.T, dir, identityPath string, repair bool) []byte {
	t.Helper()

	if repair {
		// -p (documented step 4) purges PAR2 artifacts so the archive.[0-9]*
		// reassembly glob matches only clean slices.
		runRecoveryCmd(t, dir, "par2", "repair", "-p", "archive.par2")
	}

	slices := parSlices(t, dir)
	stream := concatSlices(t, slices)

	agePath := filepath.Join(dir, "archive.age")
	require.NoError(t, os.WriteFile(agePath, stream, 0o644))

	zstPath := filepath.Join(dir, "archive.tar.zst")
	runRecoveryCmd(t, dir, "age", "-d", "-i", identityPath, "-o", zstPath, agePath)

	tarPath := filepath.Join(dir, "archive.tar")
	runRecoveryCmd(t, dir, "zstd", "-d", "-f", zstPath, "-o", tarPath)

	out := t.TempDir()
	runRecoveryCmd(t, dir, "tar", "-xf", tarPath, "-C", out)

	got, err := os.ReadFile(filepath.Join(out, "payload.bin"))
	require.NoError(t, err)

	return got
}

func requireRecoveryTools(t *testing.T) {
	t.Helper()

	for _, tool := range []string{"par2", "age", "age-keygen", "zstd", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("recovery tool %q not on PATH: %v", tool, err)
		}
	}
}

// generateAgeKeypair writes a post-quantum age identity and returns its path and
// recipient.
func generateAgeKeypair(t *testing.T) (identityPath, recipient string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "age-keygen", "-pq").CombinedOutput()
	require.NoErrorf(t, err, "age-keygen: %s", out)

	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "# public key: "):
			recipient = strings.TrimPrefix(line, "# public key: ")
		case strings.HasPrefix(line, "AGE-SECRET-KEY"):
			identityPath = filepath.Join(t.TempDir(), "identity.txt")
			require.NoError(t, os.WriteFile(identityPath, []byte(strings.TrimSpace(line)+"\n"), 0o600))
		}
	}

	require.NotEmpty(t, recipient, "age-keygen must print a recipient")
	require.NotEmpty(t, identityPath, "age-keygen must print an identity")

	return identityPath, recipient
}

func runRecoveryCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "%s %v: %s", name, args, out)
}

// parSlices returns the archive.NNN slice files in dir, sorted by name.
func parSlices(t *testing.T, dir string) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, "archive.[0-9]*"))
	require.NoError(t, err)
	require.NotEmpty(t, matches)

	return matches
}

// concatSlices concatenates the slice files in order and returns the bytes.
func concatSlices(t *testing.T, slices []string) []byte {
	t.Helper()

	var buf bytes.Buffer

	for _, slice := range slices {
		data, err := os.ReadFile(slice)
		require.NoError(t, err)
		buf.Write(data)
	}

	return buf.Bytes()
}

// corruptFile flips count bytes at offset in the named file.
func corruptFile(t *testing.T, name string, offset, count int) {
	t.Helper()

	data, err := os.ReadFile(name)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(data), offset+count, "file too small to corrupt at that offset")

	for i := offset; i < offset+count; i++ {
		data[i] ^= 0xFF
	}

	require.NoError(t, os.WriteFile(name, data, 0o644))
}

// fileSize returns the size of the named file.
func fileSize(t *testing.T, name string) int {
	t.Helper()

	info, err := os.Stat(name)
	require.NoError(t, err)

	return int(info.Size())
}

// pseudoRandom returns n incompressible xorshift bytes.
func pseudoRandom(n int) []byte {
	buf := make([]byte, n)
	state := uint64(0x9e3779b97f4a7c15)

	for i := range buf {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		buf[i] = byte(state)
	}

	return buf
}
