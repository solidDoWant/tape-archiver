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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// zstdMagic (RFC 8878) proves whether the zstd stage ran.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// smallSlice forces several slices from a modest payload.
const smallSlice = 256

// fakeLocator serves canned directories keyed by ZFSPath; unknown paths error.
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
				},
				Archives:       []ResolvedArchive{archiveOne},
				SliceSizeBytes: smallSlice,
			}

			staged, err := activities.prepare(t.Context(), stagingDir, input, nil)
			require.NoError(t, err)

			require.Len(t, staged, 1)
			result := staged[0]
			assert.Equal(t, 0, result.SourceIndex)
			require.NotEmpty(t, result.Slices, "the pipeline must produce at least one slice")

			archiveDir := filepath.Join(stagingDir, "000")

			var measured int64

			for index, slice := range result.Slices {
				// Slices stay under the staging directory — nothing goes to tape (AC4).
				assert.Equal(t, filepath.Join(archiveDir, fmt.Sprintf("%s.%03d", sliceBaseName, index)), slice.Path)
				assert.True(t, strings.HasPrefix(slice.Path, archiveDir), "slice must be staged under the archive directory")

				info, statErr := os.Stat(slice.Path)
				require.NoError(t, statErr, "every recorded slice must exist on disk")
				assert.Equal(t, info.Size(), slice.SizeBytes, "recorded slice size must match the file")

				assert.Equal(t, sha256OfFile(t, slice.Path), slice.SHA256, "recorded checksum must match the file")

				measured += info.Size()
			}

			// AC3: the recorded total is the measured on-disk size.
			assert.Equal(t, measured, result.SizeBytes)

			// Reverse the pipeline with the recoverer's tools.
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

// TestPrepareGroupArchive: a group is one archive, one subdirectory per member
// (SPEC §5), namespace-qualified so same-named PVCs never collide (#444).
func TestPrepareGroupArchive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		snapshots []ResolvedSnapshot
		want      map[string]string
	}{
		{
			name: "same PVC name in different namespaces",
			snapshots: []ResolvedSnapshot{
				{ZFSPath: "pool/a@snap", Namespace: "apps", PVC: "data"},
				{ZFSPath: "pool/b@snap", Namespace: "media", PVC: "data"},
			},
			want: map[string]string{
				"apps_data/data.txt":  "from volume a",
				"media_data/data.txt": "from volume b",
			},
		},
		{
			name: "distinct PVCs in one namespace",
			snapshots: []ResolvedSnapshot{
				{ZFSPath: "pool/a@snap", Namespace: "apps", PVC: "pvc-a"},
				{ZFSPath: "pool/b@snap", Namespace: "apps", PVC: "pvc-b"},
			},
			want: map[string]string{
				"apps_pvc-a/data.txt": "from volume a",
				"apps_pvc-b/data.txt": "from volume b",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			identity, recipient := generateKeypair(t)

			dirA := writeTree(t, map[string]string{"data.txt": "from volume a"})
			dirB := writeTree(t, map[string]string{"data.txt": "from volume b"})

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
				},
				Archives:       []ResolvedArchive{{SourceIndex: 0, Snapshots: test.snapshots}},
				SliceSizeBytes: 1 << 20,
			}

			staged, err := activities.prepare(t.Context(), t.TempDir(), input, nil)
			require.NoError(t, err)
			require.Len(t, staged, 1)

			tarBytes := decryptWithAge(t, concatSlices(t, staged[0].Slices), identity)

			assert.Equal(t, test.want, readTarFiles(t, tarBytes))
		})
	}
}

// TestMemberName: k8s members are "<namespace>_<volume>"; raw ZFS members keep
// the dataset's last component (#444).
func TestMemberName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		snapshot ResolvedSnapshot
		want     string
	}{
		{
			name:     "k8s snapshot is namespace-qualified",
			snapshot: ResolvedSnapshot{ZFSPath: "pool/k8s/pvc-1@snap", Dataset: "pool/k8s/pvc-1", SnapshotName: "snap", Namespace: "apps", PVC: "data"},
			want:     "apps_data",
		},
		{
			name:     "same PVC in another namespace differs",
			snapshot: ResolvedSnapshot{ZFSPath: "pool/k8s/pvc-2@snap", Dataset: "pool/k8s/pvc-2", SnapshotName: "snap", Namespace: "media", PVC: "data"},
			want:     "media_data",
		},
		{
			name:     "k8s snapshot without a source PVC falls back to its dataset",
			snapshot: ResolvedSnapshot{ZFSPath: "pool/k8s/pvc-3@snap", Dataset: "pool/k8s/pvc-3", SnapshotName: "snap", Namespace: "apps"},
			want:     "apps_pvc-3",
		},
		{
			name:     "PersistentVolumeClaim run-scoped snapshot",
			snapshot: ResolvedSnapshot{ZFSPath: "pool/k8s/pvc-4@tape-archiver-run", Dataset: "pool/k8s/pvc-4", SnapshotName: "tape-archiver-run", Namespace: "media", PVC: "library", RunScoped: true},
			want:     "media_library",
		},
		{
			name:     "raw ZFS snapshot",
			snapshot: ResolvedSnapshot{ZFSPath: "bulk-pool-01/archive@daily"},
			want:     "archive",
		},
		{
			name:     "raw ZFS dataset",
			snapshot: ResolvedSnapshot{ZFSPath: "bulk-pool-01/media"},
			want:     "media",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, memberName(test.snapshot))
		})
	}
}

// TestPrepareGroupArchiveDuplicateMember: two snapshots of one PVC derive the same
// subdirectory, and Prepare must fail naming them, before any tape write (AC2).
func TestPrepareGroupArchiveDuplicateMember(t *testing.T) {
	t.Parallel()

	_, recipient := generateKeypair(t)

	dirA := writeTree(t, map[string]string{"data.txt": "from snapshot a"})
	dirB := writeTree(t, map[string]string{"data.txt": "from snapshot b"})

	groupArchive := ResolvedArchive{
		SourceIndex: 0,
		Snapshots: []ResolvedSnapshot{
			{ZFSPath: "pool/a@snap", PVC: "shared-pvc"},
			{ZFSPath: "pool/b@snap", PVC: "shared-pvc"},
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
		},
		Archives:       []ResolvedArchive{groupArchive},
		SliceSizeBytes: 1 << 20,
	}

	staged, err := activities.prepare(t.Context(), t.TempDir(), input, nil)
	require.Error(t, err, "colliding group members must fail Prepare before any tape is written")
	assert.Contains(t, err.Error(), "shared-pvc", "the error must identify the colliding member subdirectory")
	assert.Contains(t, err.Error(), "sources[0]", "the error must identify the failing archive")
	assert.Empty(t, staged, "no archive may be staged when a member collision is detected")
}

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
		},
		Archives: []ResolvedArchive{
			{SourceIndex: 0, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/zero@s"}}},
			{SourceIndex: 1, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/one@s"}}},
		},
		SliceSizeBytes: 1 << 20,
	}

	staged, err := activities.prepare(t.Context(), stagingDir, input, nil)
	require.NoError(t, err)
	require.Len(t, staged, 2)

	assert.Equal(t, 0, staged[0].SourceIndex)
	assert.Equal(t, 1, staged[1].SourceIndex)
	assert.True(t, strings.HasPrefix(staged[0].Slices[0].Path, filepath.Join(stagingDir, "000")))
	assert.True(t, strings.HasPrefix(staged[1].Slices[0].Path, filepath.Join(stagingDir, "001")))
}

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
		},
		Archives: []ResolvedArchive{
			{SourceIndex: 3, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/x@s"}}},
		},
		SliceSizeBytes: 1 << 20,
	}

	_, err := activities.prepare(t.Context(), t.TempDir(), input, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sources[3]")
}

// TestPrepareInvalidConfig: a non-positive slice size would otherwise surface as a
// broken pipe, and no recipients must be rejected before plaintext reaches age.
func TestPrepareInvalidConfig(t *testing.T) {
	t.Parallel()

	_, recipient := generateKeypair(t)

	snapshotDir := writeTree(t, map[string]string{"file.txt": "data"})

	tests := []struct {
		name       string
		sliceSize  int64
		encryption config.Encryption
		wantErr    string
	}{
		{
			name:       "non-positive slice size",
			sliceSize:  0,
			encryption: config.Encryption{Recipients: []string{recipient}},
			wantErr:    "slice size must be positive",
		},
		{
			name:       "no recipients",
			sliceSize:  1 << 20,
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
				Config:         config.Config{Encryption: test.encryption},
				Archives:       []ResolvedArchive{{SourceIndex: 0, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/x@s"}}}},
				SliceSizeBytes: test.sliceSize,
			}

			_, err := activities.prepare(t.Context(), t.TempDir(), input, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
		})
	}
}

func TestPrepareArchivesRequiresStagingDir(t *testing.T) {
	t.Parallel()

	activities := newPrepareActivities("")

	_, err := activities.PrepareArchives(t.Context(), PrepareInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "staging directory")

	// Must be non-retryable so the run fails fast, not at the 24h timeout.
	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr)
	assert.True(t, appErr.NonRetryable(), "unconfigured staging root must be non-retryable")
}

// TestPrepareProgressTracker uses an injected clock so the ETA is deterministic.
func TestPrepareProgressTracker(t *testing.T) {
	t.Parallel()

	current := time.Unix(0, 0)
	clock := func() time.Time { return current }
	advance := func(elapsed time.Duration) { current = current.Add(elapsed) }

	// 100 + 300 source bytes, so progress is genuinely byte-weighted.
	archives := []ResolvedArchive{
		{SourceIndex: 3, Label: "first", LogicalBytes: 100},
		{SourceIndex: 7, Label: "second", LogicalBytes: 300},
	}

	tracker := newPrepareProgressTracker(archives, clock)

	assert.Equal(t, PrepareProgress{TotalArchives: 2, TotalSourceBytes: 400}, tracker.snapshot())

	// 50/400 = 12.5% after 10s → remaining 350 bytes at 5 B/s = 70s.
	tracker.startArchive(3, "first", 100)
	tracker.addSourceBytes(30)
	tracker.addSourceBytes(20)
	advance(10 * time.Second)

	assert.Equal(t, PrepareProgress{
		TotalArchives:          2,
		CurrentSourceIndex:     3,
		CurrentLabel:           "first",
		CurrentStage:           prepareStageStreaming,
		CurrentArchivePercent:  50,
		SourceBytesRead:        50,
		TotalSourceBytes:       400,
		OverallPercent:         12.5,
		EstimatedTimeRemaining: 70 * time.Second,
	}, tracker.snapshot())

	tracker.addSourceBytes(50)
	tracker.startChecksums(4)
	tracker.finishSlice()

	got := tracker.snapshot()
	assert.Equal(t, prepareStageChecksumming, got.CurrentStage)
	assert.Equal(t, 1, got.ChecksummedSlices)
	assert.Equal(t, 4, got.TotalSlices)
	assert.Equal(t, 100.0, got.CurrentArchivePercent, "every source byte of the archive has been read")

	tracker.finishArchive()

	got = tracker.snapshot()
	assert.Equal(t, 1, got.CompletedArchives)
	assert.Zero(t, got.CurrentSourceIndex, "the in-flight slot clears between archives")
	assert.Empty(t, got.CurrentStage, "no stage is reported between archives")
	assert.Equal(t, 25.0, got.OverallPercent, "100 of 400 source bytes are done")

	// Tar headers/padding push readBytes past the logical size; must not exceed 100%.
	tracker.startArchive(7, "second", 300)
	tracker.addSourceBytes(320)

	assert.Equal(t, 100.0, tracker.snapshot().CurrentArchivePercent, "an over-read clamps to 100%")

	tracker.finishArchive()

	got = tracker.snapshot()
	assert.Equal(t, 2, got.CompletedArchives)
	assert.Equal(t, 100.0, got.OverallPercent)
	assert.Equal(t, int64(400), got.SourceBytesRead)
	assert.Zero(t, got.EstimatedTimeRemaining, "no time remains once every archive is staged")

	var nilTracker *prepareProgressTracker

	assert.NotPanics(t, func() {
		nilTracker.startArchive(1, "none", 100)
		nilTracker.addSourceBytes(10)
		nilTracker.startChecksums(2)
		nilTracker.finishSlice()
		nilTracker.finishArchive()
	})

	var sink bytes.Buffer
	assert.Same(t, &sink, nilTracker.countingWriter(&sink), "a nil tracker must not wrap the tar stage")
}

// TestPrepareProgressMessage: the web log panel shows only message text, so the
// numbers must be inline.
func TestPrepareProgressMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		progress PrepareProgress
		want     string
	}{
		{
			name:     "before the first archive starts",
			progress: PrepareProgress{TotalArchives: 3, TotalSourceBytes: 400},
			want:     "prepare: progress 0.0% overall (0/3 archives staged)",
		},
		{
			name: "streaming an archive, with an ETA",
			progress: PrepareProgress{
				TotalArchives: 3, CompletedArchives: 1,
				CurrentSourceIndex: 1, CurrentLabel: "db", CurrentStage: prepareStageStreaming,
				CurrentArchivePercent: 42.5, OverallPercent: 61.3,
				EstimatedTimeRemaining: 90 * time.Minute,
			},
			want: `prepare: progress 61.3% overall (1/3 archives staged, streaming "db" at 42.5%), eta 1h30m0s`,
		},
		{
			name: "checksumming an archive's slices",
			progress: PrepareProgress{
				TotalArchives: 3, CompletedArchives: 1,
				CurrentSourceIndex: 1, CurrentLabel: "db", CurrentStage: prepareStageChecksumming,
				CurrentArchivePercent: 100, ChecksummedSlices: 3, TotalSlices: 12,
				OverallPercent: 66.6,
			},
			want: `prepare: progress 66.6% overall (1/3 archives staged, checksumming "db" slice 3/12)`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, prepareProgressMessage(test.progress))
		})
	}
}

func TestPrepareReportsProgress(t *testing.T) {
	t.Parallel()

	_, recipient := generateKeypair(t)

	dirZero := writeTree(t, map[string]string{"zero.txt": strings.Repeat("zero", 500)})
	dirOne := writeTree(t, map[string]string{"one.txt": strings.Repeat("one!", 500)})

	archives := []ResolvedArchive{
		{SourceIndex: 0, Label: "zero", LogicalBytes: 2000, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/zero@snap"}}},
		{SourceIndex: 1, Label: "one", LogicalBytes: 2000, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/one@snap"}}},
	}

	activities := &PrepareActivities{
		stagingRoot: t.TempDir(),
		locator:     fakeLocator{dirs: map[string]string{"pool/zero@snap": dirZero, "pool/one@snap": dirOne}},
	}

	input := PrepareInput{
		Config:         config.Config{Encryption: config.Encryption{Recipients: []string{recipient}}},
		Archives:       archives,
		SliceSizeBytes: smallSlice,
	}

	tracker := newPrepareProgressTracker(archives, time.Now)

	staged, err := activities.prepare(t.Context(), t.TempDir(), input, tracker)
	require.NoError(t, err)
	require.Len(t, staged, len(archives))

	got := tracker.snapshot()
	assert.Equal(t, len(archives), got.TotalArchives)
	assert.Equal(t, len(archives), got.CompletedArchives, "every archive must be counted complete")
	assert.Equal(t, 100.0, got.OverallPercent, "all source bytes are done once every archive is staged")
	assert.Zero(t, got.EstimatedTimeRemaining, "no time remains once the phase is done")
}

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

// sha256OfFile is independent of the production checksum helper.
func sha256OfFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

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
