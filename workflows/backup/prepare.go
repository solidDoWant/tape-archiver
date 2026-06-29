package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"
	"golang.org/x/sync/errgroup"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/agewrap"
	"github.com/solidDoWant/tape-archiver/pkg/archive"
	"github.com/solidDoWant/tape-archiver/pkg/checksum"
	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// The Prepare phase (SPEC §4.3 phase 2) turns the resolved work list into staged,
// tape-ready files on disk. For each resolved archive it streams the per-archive
// pipeline — tar the snapshot tree → optional zstd → age-encrypt → split into
// fixed-size slices — computes a SHA-256 per slice, and measures the exact on-disk
// size. Nothing is written to tape: every byte lands under the data worker's
// staging directory so the later write window is a pure disk→tape copy (SPEC §4.3,
// §14). The measured size, not the Resolve estimate, is what the Pack phase plans
// against.
//
// It runs entirely on the data worker, where the snapshot bytes live (SPEC §4.1).

const (
	// prepareTimeout bounds the whole Prepare activity. Unlike Resolve's cheap
	// metadata reads, Prepare reads, compresses, encrypts, and slices the full
	// snapshot contents — potentially terabytes — so the bound is deliberately
	// generous. The streaming stages honor context cancellation per read buffer,
	// so a cancelled or timed-out run stops promptly rather than at end-of-copy.
	prepareTimeout = 24 * time.Hour

	// stagingDirPerm is the permission staging directories are created with. The
	// staging tree holds encrypted archive slices on the data worker only.
	stagingDirPerm = 0o755

	// sliceBaseName is the base file name for an archive's slice files within its
	// staging directory; Split appends the ".NNN" slice index. Each archive has
	// its own directory, so a fixed base name keeps slice names short and uniform.
	sliceBaseName = "archive"
)

// snapshotLocator resolves a resolved snapshot to the on-disk directory whose
// contents the Prepare phase tars. It is the seam the zfs package functions are
// wrapped behind, injected so the activity is unit-testable without a ZFS pool.
type snapshotLocator interface {
	SnapshotDir(ctx context.Context, snapshot ResolvedSnapshot) (string, error)
}

// zfsLocator is the production snapshotLocator: it reads the dataset's mountpoint
// from ZFS and resolves the snapshot's .zfs/snapshot/<snap>/ directory under it
// (SPEC §6). A raw ZFS source naming a dataset rather than a snapshot (no "@")
// resolves to the dataset's live mountpoint.
type zfsLocator struct{}

func (zfsLocator) SnapshotDir(ctx context.Context, snapshot ResolvedSnapshot) (string, error) {
	dataset, snapName := datasetAndSnapshot(snapshot)

	mount, err := zfs.Mountpoint(ctx, dataset)
	if err != nil {
		return "", fmt.Errorf("resolve mountpoint for %q: %w", dataset, err)
	}

	if snapName == "" {
		return mount, nil
	}

	return zfs.SnapshotDir(mount, snapName)
}

// PrepareActivities hosts the data-side Prepare activity. stagingRoot is the
// directory all runs stage under (operational worker config, SPEC §4.1); each run
// isolates its output beneath it by run id.
type PrepareActivities struct {
	stagingRoot string
	locator     snapshotLocator
}

// newPrepareActivities returns the production data-side Prepare activity, staging
// into stagingRoot and locating snapshot contents through the zfs CLI.
func newPrepareActivities(stagingRoot string) *PrepareActivities {
	return &PrepareActivities{stagingRoot: stagingRoot, locator: zfsLocator{}}
}

// PrepareInput is the payload for the Prepare activity: the run config (for the
// recipients and slice size) and the resolved work list to stage.
type PrepareInput struct {
	Config   config.Config
	Archives []ResolvedArchive
}

// PrepareArchives stages every resolved archive to disk (SPEC §4.3 phase 2),
// returning a StagedArchive per input archive in the same order, each with its
// slice paths, per-slice SHA-256, and measured total size. Output is isolated
// under a per-run subdirectory of the staging root so concurrent or repeated runs
// never collide; the run id comes from the activity context.
func (a *PrepareActivities) PrepareArchives(ctx context.Context, input PrepareInput) ([]StagedArchive, error) {
	if a.stagingRoot == "" {
		return nil, fmt.Errorf("staging directory is not configured (set TAPE_STAGING_DIR on the data worker)")
	}

	stagingDir := filepath.Join(a.stagingRoot, activity.GetInfo(ctx).WorkflowExecution.RunID)

	return a.prepare(ctx, stagingDir, input)
}

// prepare stages every archive under stagingDir. It is split from
// PrepareArchives so it can be exercised against a real directory without an
// activity context.
func (a *PrepareActivities) prepare(ctx context.Context, stagingDir string, input PrepareInput) ([]StagedArchive, error) {
	recipients := input.Config.Encryption.Recipients
	sliceSize := input.Config.Redundancy.SliceSizeBytes

	staged := make([]StagedArchive, 0, len(input.Archives))

	for _, resolvedArchive := range input.Archives {
		result, err := a.stageArchive(ctx, stagingDir, resolvedArchive, recipients, sliceSize)
		if err != nil {
			return nil, fmt.Errorf("prepare sources[%d]: %w", resolvedArchive.SourceIndex, err)
		}

		staged = append(staged, result)
	}

	return staged, nil
}

// stageArchive runs the per-archive prepare pipeline and measures its output. It
// creates the archive's staging directory, streams tar → optional zstd → age →
// split into it, then stats and checksums each slice to produce the StagedArchive.
func (a *PrepareActivities) stageArchive(ctx context.Context, stagingDir string, resolvedArchive ResolvedArchive, recipients []string, sliceSize int64) (StagedArchive, error) {
	archiveDir := filepath.Join(stagingDir, fmt.Sprintf("%03d", resolvedArchive.SourceIndex))
	if err := os.MkdirAll(archiveDir, stagingDirPerm); err != nil {
		return StagedArchive{}, fmt.Errorf("create staging directory %q: %w", archiveDir, err)
	}

	members, err := a.resolveMembers(ctx, resolvedArchive)
	if err != nil {
		return StagedArchive{}, err
	}

	slicePaths, err := runPipeline(ctx, archiveDir, members, resolvedArchive.Compression, recipients, sliceSize)
	if err != nil {
		return StagedArchive{}, err
	}

	slices := make([]StagedSlice, 0, len(slicePaths))

	var total int64

	for _, slicePath := range slicePaths {
		info, err := os.Stat(slicePath)
		if err != nil {
			return StagedArchive{}, fmt.Errorf("measure slice %q: %w", slicePath, err)
		}

		digest, err := checksum.SHA256File(slicePath)
		if err != nil {
			return StagedArchive{}, fmt.Errorf("checksum slice %q: %w", slicePath, err)
		}

		slices = append(slices, StagedSlice{Path: slicePath, SHA256: digest, SizeBytes: info.Size()})
		total += info.Size()
	}

	return StagedArchive{SourceIndex: resolvedArchive.SourceIndex, Slices: slices, SizeBytes: total}, nil
}

// resolveMembers turns an archive's resolved snapshots into the tar members to
// pack, each located on disk via the snapshotLocator.
func (a *PrepareActivities) resolveMembers(ctx context.Context, resolvedArchive ResolvedArchive) ([]archive.Member, error) {
	members := make([]archive.Member, 0, len(resolvedArchive.Snapshots))

	for _, snapshot := range resolvedArchive.Snapshots {
		dir, err := a.locator.SnapshotDir(ctx, snapshot)
		if err != nil {
			return nil, fmt.Errorf("locate snapshot %q: %w", snapshot.ZFSPath, err)
		}

		members = append(members, archive.Member{Subdir: memberName(snapshot), Dir: dir})
	}

	return members, nil
}

// runPipeline streams the per-archive prepare stages into archiveDir and returns
// the staged slice paths in order. The stages are wired together with io.Pipe —
// tar (single tree, or one subdirectory per member for a group) → optional zstd →
// age → split — and run concurrently in an errgroup so the whole archive flows
// through without staging any intermediate result to disk. Only the final slices
// are written, by Split.
func runPipeline(ctx context.Context, archiveDir string, members []archive.Member, compress bool, recipients []string, sliceSize int64) ([]string, error) {
	// Validate up front: a non-positive slice size makes Split fail before it
	// reads, which would otherwise surface only as a broken-pipe error from the
	// upstream stages rather than this clear cause.
	if sliceSize <= 0 {
		return nil, fmt.Errorf("slice size must be positive, got %d", sliceSize)
	}

	group, groupCtx := errgroup.WithContext(ctx)

	tarReader := pipeStage(group, nil, func(w io.Writer) error {
		return tarMembers(groupCtx, w, members)
	})

	encryptInput := tarReader

	if compress {
		encryptInput = pipeStage(group, tarReader, func(w io.Writer) error {
			return archive.Compress(groupCtx, w, tarReader)
		})
	}

	encryptReader := pipeStage(group, encryptInput, func(w io.Writer) error {
		return agewrap.Encrypt(groupCtx, w, encryptInput, recipients...)
	})

	slicePaths, splitErr := archive.Split(groupCtx, encryptReader, sliceSize, archiveDir, sliceBaseName)

	// Unblock the encrypt stage if Split stopped reading early (an error); on
	// success Split has already drained the stream to EOF and this is a no-op.
	encryptReader.CloseWithError(splitErr)

	if waitErr := group.Wait(); waitErr != nil {
		return nil, waitErr
	}

	if splitErr != nil {
		return nil, fmt.Errorf("split archive: %w", splitErr)
	}

	return slicePaths, nil
}

// pipeStage runs produce in the group, writing to a fresh pipe whose read end is
// returned for the next stage to consume. On return it closes the write end with
// produce's error (signalling EOF or failure downstream) and, when this stage has
// an input reader, closes that reader with the same error so a failure unblocks
// the upstream producer rather than deadlocking it on a full pipe.
func pipeStage(group *errgroup.Group, input *io.PipeReader, produce func(io.Writer) error) *io.PipeReader {
	reader, writer := io.Pipe()

	group.Go(func() error {
		err := produce(writer)

		_ = writer.CloseWithError(err)

		if input != nil {
			_ = input.CloseWithError(err)
		}

		return err
	})

	return reader
}

// tarMembers tars an archive's members. A single member is tarred directly so the
// snapshot's contents sit at the archive root (SPEC §4.3 phase 2); a group of
// members is tarred with one subdirectory per member for cross-volume consistency
// (SPEC §5).
func tarMembers(ctx context.Context, w io.Writer, members []archive.Member) error {
	if len(members) == 1 {
		return archive.Tar(ctx, w, members[0].Dir)
	}

	return archive.TarMembers(ctx, w, members)
}

// datasetAndSnapshot splits a resolved snapshot into its dataset and short
// snapshot name. A k8s-resolved snapshot carries them explicitly; a raw ZFS
// source carries only ZFSPath, split on the "@" that separates a snapshot from
// its dataset. A raw path with no "@" is a dataset, returned with an empty
// snapshot name.
func datasetAndSnapshot(snapshot ResolvedSnapshot) (dataset, snapName string) {
	if snapshot.Dataset != "" {
		return snapshot.Dataset, snapshot.SnapshotName
	}

	if before, after, found := strings.Cut(snapshot.ZFSPath, "@"); found {
		return before, after
	}

	return snapshot.ZFSPath, ""
}

// memberName is the subdirectory a group member's contents are tarred under. The
// PVC name is the natural volume identity for a k8s member; a raw ZFS source
// falls back to the dataset's last path component. It is only meaningful for
// multi-member groups — a single-member archive is tarred without a subdirectory.
func memberName(snapshot ResolvedSnapshot) string {
	if snapshot.PVC != "" {
		return snapshot.PVC
	}

	dataset, _ := datasetAndSnapshot(snapshot)

	return filepath.Base(dataset)
}

// preparePhase orchestrates the Prepare phase (SPEC §4.3 phase 2): it runs the
// data-side Prepare activity over the resolved work list and stores the staged
// archives in runState for the Pack phase. A failure aborts the run here, before
// any tape is touched.
func preparePhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: prepareTimeout,
	})

	var activities *PrepareActivities

	input := PrepareInput{Config: cfg, Archives: state.resolved}

	var staged []StagedArchive
	if err := workflow.ExecuteActivity(dataCtx, activities.PrepareArchives, input).Get(dataCtx, &staged); err != nil {
		return err
	}

	state.staged = staged

	return nil
}
