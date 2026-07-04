package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// zstdMagic is the zstd frame magic number (RFC 8878), little-endian. The
// compressed-vs-uncompressed cases assert on its presence in the decrypted
// stream to prove whether the zstd stage ran.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// smallSlice forces several slices from a modest payload so the split, ordering,
// per-slice checksum, and size-measurement paths are all exercised.
const smallSlice = 256

// fakeLocator is a snapshotLocator serving canned on-disk directories keyed by a
// snapshot's ZFSPath, so the Prepare pipeline runs against temp directories
// without a ZFS pool. A ZFSPath absent from the map errors.
type fakeLocator struct {
	dirs map[string]string
	err  error
}

func (f fakeLocator) SnapshotDir(_ context.Context, snapshot ResolvedSnapshot) (string, error) {
	if f.err != nil {
		return "", f.err
	}

	dir, ok := f.dirs[snapshot.ZFSPath]
	if !ok {
		return "", errors.New("no directory for " + snapshot.ZFSPath)
	}

	return dir, nil
}

func TestPrepareArchivePipeline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		compression bool
	}{
		{name: "compression enabled", compression: true},
		{name: "compression disabled", compression: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			identity, recipient := generateKeypair(t)

			// A snapshot tree with nested files; large enough to span several
			// slices once encrypted.
			contents := map[string]string{
				"file.txt":            strings.Repeat("alpha-", 200),
				"nested/inner.bin":    strings.Repeat("beta!", 300),
				"nested/deep/last.go": "package main\n",
			}
			snapshotDir := writeTree(t, contents)

			archiveOne := ResolvedArchive{
				SourceIndex: 0,
				Compression: test.compression,
				Snapshots:   []ResolvedSnapshot{{ZFSPath: "bulk-pool-01/archive@daily"}},
			}

			activities := &PrepareActivities{
				stagingRoot: t.TempDir(),
				locator:     fakeLocator{dirs: map[string]string{"bulk-pool-01/archive@daily": snapshotDir}},
			}

			stagingDir := t.TempDir()
			input := PrepareInput{
				Config: config.Config{
					Encryption: config.Encryption{Recipients: []string{recipient}},
					Redundancy: config.Redundancy{SliceSizeBytes: smallSlice},
				},
				Archives: []ResolvedArchive{archiveOne},
			}

			staged, err := activities.prepare(t.Context(), stagingDir, input)
			require.NoError(t, err)

			require.Len(t, staged, 1)
			result := staged[0]
			assert.Equal(t, 0, result.SourceIndex)
			require.NotEmpty(t, result.Slices, "the pipeline must produce at least one slice")

			archiveDir := filepath.Join(stagingDir, "000")

			var measured int64

			for index, slice := range result.Slices {
				// Slices are named and ordered, and live under the archive's
				// staging directory — nothing is written to tape (AC4).
				assert.Equal(t, filepath.Join(archiveDir, fmt.Sprintf("%s.%03d", sliceBaseName, index)), slice.Path)
				assert.True(t, strings.HasPrefix(slice.Path, archiveDir), "slice must be staged under the archive directory")

				info, statErr := os.Stat(slice.Path)
				require.NoError(t, statErr, "every recorded slice must exist on disk")
				assert.Equal(t, info.Size(), slice.SizeBytes, "recorded slice size must match the file")

				assert.Equal(t, sha256OfFile(t, slice.Path), slice.SHA256, "recorded checksum must match the file")

				measured += info.Size()
			}

			// The recorded total is the measured on-disk size (AC3).
			assert.Equal(t, measured, result.SizeBytes)

			// Reverse the pipeline with the same tools a recoverer uses and
			// confirm the original tree round-trips exactly.
			encrypted := concatSlices(t, result.Slices)
			decrypted := decryptWithAge(t, encrypted, identity)

			tarBytes := decrypted
			if test.compression {
				assert.Equal(t, zstdMagic, decrypted[:len(zstdMagic)], "compression enabled: decrypted stream must be zstd")
				tarBytes = zstdDecompress(t, decrypted)
			} else {
				assert.NotEqual(t, zstdMagic, decrypted[:len(zstdMagic)], "compression disabled: decrypted stream must not be zstd")
			}

			assert.Equal(t, contents, readTarFiles(t, tarBytes))
		})
	}
}

// TestPrepareGroupArchive verifies a multi-snapshot archive (a k8s group) is
// staged as one archive with one subdirectory per member (SPEC §5).
func TestPrepareGroupArchive(t *testing.T) {
	t.Parallel()

	identity, recipient := generateKeypair(t)

	dirA := writeTree(t, map[string]string{"data.txt": "from volume a"})
	dirB := writeTree(t, map[string]string{"data.txt": "from volume b"})

	groupArchive := ResolvedArchive{
		SourceIndex: 0,
		Compression: false,
		Snapshots: []ResolvedSnapshot{
			{ZFSPath: "pool/a@snap", PVC: "pvc-a"},
			{ZFSPath: "pool/b@snap", PVC: "pvc-b"},
		},
	}

	activities := &PrepareActivities{
		stagingRoot: t.TempDir(),
		locator: fakeLocator{dirs: map[string]string{
			"pool/a@snap": dirA,
			"pool/b@snap": dirB,
		}},
	}

	input := PrepareInput{
		Config: config.Config{
			Encryption: config.Encryption{Recipients: []string{recipient}},
			Redundancy: config.Redundancy{SliceSizeBytes: 1 << 20},
		},
		Archives: []ResolvedArchive{groupArchive},
	}

	staged, err := activities.prepare(t.Context(), t.TempDir(), input)
	require.NoError(t, err)
	require.Len(t, staged, 1)

	tarBytes := decryptWithAge(t, concatSlices(t, staged[0].Slices), identity)

	assert.Equal(t, map[string]string{
		"pvc-a/data.txt": "from volume a",
		"pvc-b/data.txt": "from volume b",
	}, readTarFiles(t, tarBytes))
}

// TestPrepareMultipleArchives verifies each resolved archive is staged into its
// own directory and recorded in order.
func TestPrepareMultipleArchives(t *testing.T) {
	t.Parallel()

	_, recipient := generateKeypair(t)

	dirZero := writeTree(t, map[string]string{"zero.txt": "zero"})
	dirOne := writeTree(t, map[string]string{"one.txt": "one"})

	activities := &PrepareActivities{
		stagingRoot: t.TempDir(),
		locator: fakeLocator{dirs: map[string]string{
			"pool/zero@s": dirZero,
			"pool/one@s":  dirOne,
		}},
	}

	stagingDir := t.TempDir()
	input := PrepareInput{
		Config: config.Config{
			Encryption: config.Encryption{Recipients: []string{recipient}},
			Redundancy: config.Redundancy{SliceSizeBytes: 1 << 20},
		},
		Archives: []ResolvedArchive{
			{SourceIndex: 0, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/zero@s"}}},
			{SourceIndex: 1, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/one@s"}}},
		},
	}

	staged, err := activities.prepare(t.Context(), stagingDir, input)
	require.NoError(t, err)
	require.Len(t, staged, 2)

	assert.Equal(t, 0, staged[0].SourceIndex)
	assert.Equal(t, 1, staged[1].SourceIndex)
	assert.True(t, strings.HasPrefix(staged[0].Slices[0].Path, filepath.Join(stagingDir, "000")))
	assert.True(t, strings.HasPrefix(staged[1].Slices[0].Path, filepath.Join(stagingDir, "001")))
}

// TestPrepareLocatorError verifies a snapshot that cannot be located fails the
// archive, naming the source.
func TestPrepareLocatorError(t *testing.T) {
	t.Parallel()

	_, recipient := generateKeypair(t)

	activities := &PrepareActivities{
		stagingRoot: t.TempDir(),
		locator:     fakeLocator{err: errors.New("pool offline")},
	}

	input := PrepareInput{
		Config: config.Config{
			Encryption: config.Encryption{Recipients: []string{recipient}},
			Redundancy: config.Redundancy{SliceSizeBytes: 1 << 20},
		},
		Archives: []ResolvedArchive{
			{SourceIndex: 3, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/x@s"}}},
		},
	}

	_, err := activities.prepare(t.Context(), t.TempDir(), input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sources[3]")
}

// TestPrepareInvalidConfig verifies the pipeline fails with a clear cause for
// misconfiguration that would otherwise surface as an opaque broken-pipe error
// (a non-positive slice size) or be unsafe (no recipients, which agewrap rejects
// before any plaintext is fed to age).
func TestPrepareInvalidConfig(t *testing.T) {
	t.Parallel()

	_, recipient := generateKeypair(t)

	snapshotDir := writeTree(t, map[string]string{"file.txt": "data"})

	tests := []struct {
		name       string
		redundancy config.Redundancy
		encryption config.Encryption
		wantErr    string
	}{
		{
			name:       "non-positive slice size",
			redundancy: config.Redundancy{SliceSizeBytes: 0},
			encryption: config.Encryption{Recipients: []string{recipient}},
			wantErr:    "slice size must be positive",
		},
		{
			name:       "no recipients",
			redundancy: config.Redundancy{SliceSizeBytes: 1 << 20},
			encryption: config.Encryption{},
			wantErr:    "recipient",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			activities := &PrepareActivities{
				stagingRoot: t.TempDir(),
				locator:     fakeLocator{dirs: map[string]string{"pool/x@s": snapshotDir}},
			}

			input := PrepareInput{
				Config:   config.Config{Encryption: test.encryption, Redundancy: test.redundancy},
				Archives: []ResolvedArchive{{SourceIndex: 0, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/x@s"}}}},
			}

			_, err := activities.prepare(t.Context(), t.TempDir(), input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}

// TestPrepareArchivesRequiresStagingDir verifies the activity rejects an
// unconfigured staging root rather than staging into an unintended location.
func TestPrepareArchivesRequiresStagingDir(t *testing.T) {
	t.Parallel()

	activities := newPrepareActivities("")

	_, err := activities.PrepareArchives(t.Context(), PrepareInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "staging directory")

	// Misconfiguration recurs on every retry, so the activity must surface it as
	// non-retryable — the run fails fast rather than retrying to the 24h timeout.
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.True(t, appErr.NonRetryable(), "unconfigured staging root must be non-retryable")
}

// writeTree creates a temp directory populated with the given relative-path →
// contents map (creating parent directories as needed) and returns its path.
func writeTree(t *testing.T, contents map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for rel, data := range contents {
		path := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(data), 0o644))
	}

	return root
}

// sha256OfFile returns the lowercase hex SHA-256 of the file at path, computed
// independently of the production checksum helper.
func sha256OfFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// generateKeypair generates a fresh age post-quantum keypair with the real
// age-keygen binary and returns the identity file path and the recipient string.
func generateKeypair(t *testing.T) (identityPath, recipient string) {
	t.Helper()

	identityPath = filepath.Join(t.TempDir(), "identity.txt")

	cmd := exec.CommandContext(t.Context(), "age-keygen", "-pq", "-o", identityPath)

	var stderr strings.Builder

	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "age-keygen failed: %s", stderr.String())

	contents, err := os.ReadFile(identityPath)
	require.NoError(t, err)

	const marker = "# public key: "

	for _, line := range strings.Split(string(contents), "\n") {
		if after, found := strings.CutPrefix(line, marker); found {
			recipient = strings.TrimSpace(after)

			break
		}
	}

	require.True(t, strings.HasPrefix(recipient, "age1pq1"), "expected a post-quantum recipient, got %q", recipient)

	return identityPath, recipient
}

// decryptWithAge decrypts data with the given identity file by invoking the
// bundled age binary — the same tool a recoverer uses.
func decryptWithAge(t *testing.T, data []byte, identityPath string) []byte {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "age", "-d", "-i", identityPath)
	cmd.Stdin = bytes.NewReader(data)

	var out, stderr bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Run(), "age decrypt failed: %s", stderr.String())

	return out.Bytes()
}

// concatSlices reads the staged slices in order and concatenates them, the
// authoritative reconstruction of the archive's stream.
func concatSlices(t *testing.T, slices []StagedSlice) []byte {
	t.Helper()

	var buf bytes.Buffer

	for _, slice := range slices {
		data, err := os.ReadFile(slice.Path)
		require.NoError(t, err)
		buf.Write(data)
	}

	return buf.Bytes()
}

// zstdDecompress decompresses a zstd stream with the bundled zstd binary — the
// same tool a recoverer uses.
func zstdDecompress(t *testing.T, data []byte) []byte {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "zstd", "-d", "-q", "-c")
	cmd.Stdin = bytes.NewReader(data)

	var out, stderr bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Run(), "zstd -d failed: %s", stderr.String())

	return out.Bytes()
}

// readTarFiles extracts a tar archive into a relative-path → contents map for
// regular files, so a round-tripped tree can be compared against its source.
func readTarFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()

	reader := tar.NewReader(bytes.NewReader(data))
	files := make(map[string]string)

	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		if header.Typeflag != tar.TypeReg {
			continue
		}

		contents, err := io.ReadAll(reader)
		require.NoError(t, err)

		files[header.Name] = string(contents)
	}

	return files
}
