package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"golang.org/x/sync/errgroup"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/agewrap"
	"github.com/solidDoWant/tape-archiver/pkg/archive"
	"github.com/solidDoWant/tape-archiver/pkg/checksum"
	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// The Prepare phase (SPEC §4.3 phase 2) stages each archive to disk as
// tar → optional zstd → age → fixed-size slices, with a SHA-256 per slice.
// Nothing touches tape, so the write window is a pure disk→tape copy (SPEC §4.3,
// §14); Pack plans against the measured size, not the Resolve estimate.

const (
	// prepareTimeout is generous because Prepare streams full snapshot contents
	// (potentially terabytes); the stages honor cancellation per read buffer.
	prepareTimeout = 24 * time.Hour

	// prepareMaxAttempts bounds retries so a deterministic failure fails the run
	// instead of re-staging every earlier archive forever (#385); a heartbeat
	// timeout from a worker restart is still retried.
	prepareMaxAttempts = 3

	// datasetNotMountedErrorType marks a raw dataset source that is not mounted.
	datasetNotMountedErrorType = "dataset-not-mounted"

	stagingDirPerm = 0o755

	// sliceBaseName is the base name Split appends ".NNN" to; each archive has its
	// own directory.
	sliceBaseName = "archive"

	// The two per-archive stages reported in progress, so a long archive is
	// distinguishable from a stalled one.
	prepareStageStreaming    = "streaming"
	prepareStageChecksumming = "checksumming"
)

// snapshotLocator resolves a snapshot to the directory Prepare tars; injected so
// the activity is testable without a ZFS pool.
type snapshotLocator interface {
	SnapshotDir(ctx context.Context, snapshot ResolvedSnapshot) (string, error)
}

// zfsLocator is the production snapshotLocator (SPEC §6). A raw dataset source
// (no "@") resolves to the dataset's live mountpoint.
type zfsLocator struct{}

func (zfsLocator) SnapshotDir(ctx context.Context, snapshot ResolvedSnapshot) (string, error) {
	dataset, snapName := datasetAndSnapshot(snapshot)

	mount, err := zfs.Mountpoint(ctx, dataset)
	if err != nil {
		return "", fmt.Errorf("resolve mountpoint for %q: %w", dataset, err)
	}

	if snapName == "" || snapshot.RunScoped {
		// An unmounted dataset's mountpoint can survive as a "shadow" directory,
		// so archiving it would silently certify empty or stale contents (#135),
		// and a run-scoped snapshot of it is reachable only through its mount.
		// Fail here, before any tape is written.
		mounted, err := zfs.Mounted(ctx, dataset)
		if err != nil {
			return "", fmt.Errorf("check mount state for %q: %w", dataset, err)
		}

		if !mounted {
			return "", temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"raw ZFS dataset %q is not mounted; refusing to archive its (shadow) mountpoint %q", dataset, mount),
				datasetNotMountedErrorType, nil)
		}

		if snapName == "" {
			return mount, nil
		}
	}

	return zfs.SnapshotDir(mount, snapName)
}

// PrepareActivities hosts the data-side Prepare activity; each run stages under
// stagingRoot/<run id>.
type PrepareActivities struct {
	stagingRoot string
	locator     snapshotLocator
}

func newPrepareActivities(stagingRoot string) *PrepareActivities {
	return &PrepareActivities{stagingRoot: stagingRoot, locator: zfsLocator{}}
}

// PrepareInput is the Prepare activity payload; SliceSizeBytes comes from
// Resolve (DeriveSliceSize; #324).
type PrepareInput struct {
	Config         config.Config
	Archives       []ResolvedArchive
	SliceSizeBytes int64
}

// PrepareProgress is the Prepare heartbeat payload and progress-log snapshot.
// Progress is measured on source bytes read by tar against Resolve's
// LogicalBytes: the staged output size is unknowable in advance, so the source
// side is the only honest denominator.
type PrepareProgress struct {
	TotalArchives     int `json:"totalArchives"`
	CompletedArchives int `json:"completedArchives"`

	// The in-flight archive; zero-valued between archives.
	CurrentSourceIndex    int     `json:"currentSourceIndex"`
	CurrentLabel          string  `json:"currentLabel"`
	CurrentStage          string  `json:"currentStage"`
	CurrentArchivePercent float64 `json:"currentArchivePercent"`

	// Source bytes stop moving during the checksum pass, so these show it is
	// progressing.
	ChecksummedSlices int `json:"checksummedSlices"`
	TotalSlices       int `json:"totalSlices"`

	SourceBytesRead  int64 `json:"sourceBytesRead"`
	TotalSourceBytes int64 `json:"totalSourceBytes"`

	// EstimatedTimeRemaining projects the rate over the last etaWindow; zero
	// until there is progress to estimate from, or while progress is stalled.
	OverallPercent         float64       `json:"overallPercent"`
	EstimatedTimeRemaining time.Duration `json:"estimatedTimeRemaining"`
}

// prepareProgressTracker is advanced by the staging goroutines and snapshotted by
// the heartbeat goroutine. Mutating methods are nil-safe (the test path).
type prepareProgressTracker struct {
	// Set at construction and read without the lock.
	now           func() time.Time
	totalArchives int
	totalBytes    int64

	mu             sync.Mutex
	eta            *etaEstimator
	completedCount int
	completedBytes int64
	current        prepareCurrentArchive
}

type prepareCurrentArchive struct {
	sourceIndex  int
	label        string
	logicalBytes int64
	readBytes    int64
	stage        string
	totalSlices  int
	doneSlices   int
}

func newPrepareProgressTracker(archives []ResolvedArchive, now func() time.Time) *prepareProgressTracker {
	var totalBytes int64
	for _, resolvedArchive := range archives {
		totalBytes += resolvedArchive.LogicalBytes
	}

	return &prepareProgressTracker{
		now:           now,
		eta:           newETAEstimator(now(), etaWindow),
		totalArchives: len(archives),
		totalBytes:    totalBytes,
	}
}

func (p *prepareProgressTracker) startArchive(sourceIndex int, label string, logicalBytes int64) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current = prepareCurrentArchive{
		sourceIndex:  sourceIndex,
		label:        label,
		logicalBytes: logicalBytes,
		stage:        prepareStageStreaming,
	}
}

// addSourceBytes is called per write by the counting writer and must stay cheap.
func (p *prepareProgressTracker) addSourceBytes(n int64) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current.readBytes += n
}

func (p *prepareProgressTracker) startChecksums(totalSlices int) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current.stage = prepareStageChecksumming
	p.current.totalSlices = totalSlices
	p.current.doneSlices = 0
}

func (p *prepareProgressTracker) finishSlice() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current.doneSlices++
}

// finishArchive credits the archive's full logical size rather than the bytes
// read, so a tree that tars smaller (sparse files) still ends at 100%.
func (p *prepareProgressTracker) finishArchive() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.completedBytes += p.current.logicalBytes
	p.completedCount++
	p.current = prepareCurrentArchive{}
}

func (p *prepareProgressTracker) countingWriter(w io.Writer) io.Writer {
	if p == nil {
		return w
	}

	return &countingWriter{writer: w, count: p.addSourceBytes}
}

func (p *prepareProgressTracker) snapshot() PrepareProgress {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Clamped so tar's headers and padding cannot push the phase past 100%.
	currentBytes := min(p.current.readBytes, p.current.logicalBytes)
	doneBytes := p.completedBytes + currentBytes

	progress := PrepareProgress{
		TotalArchives:      p.totalArchives,
		CompletedArchives:  p.completedCount,
		CurrentSourceIndex: p.current.sourceIndex,
		CurrentLabel:       p.current.label,
		CurrentStage:       p.current.stage,
		ChecksummedSlices:  p.current.doneSlices,
		TotalSlices:        p.current.totalSlices,
		SourceBytesRead:    doneBytes,
		TotalSourceBytes:   p.totalBytes,
	}

	if p.current.logicalBytes > 0 {
		progress.CurrentArchivePercent = roundPercent(float64(currentBytes) / float64(p.current.logicalBytes) * 100)
	}

	if p.totalBytes > 0 {
		progress.OverallPercent = roundPercent(float64(doneBytes) / float64(p.totalBytes) * 100)

		progress.EstimatedTimeRemaining = p.eta.estimate(p.now(), doneBytes, p.totalBytes)
	}

	return progress
}

type countingWriter struct {
	writer io.Writer
	count  func(int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	written, err := c.writer.Write(p)

	// Count even on a short write, so the count never runs ahead of the stream.
	c.count(int64(written))

	return written, err
}

// PrepareArchives stages every resolved archive to disk (SPEC §4.3 phase 2),
// returning one StagedArchive per input in order.
func (a *PrepareActivities) PrepareArchives(ctx context.Context, input PrepareInput) ([]StagedArchive, error) {
	if a.stagingRoot == "" {
		// Misconfiguration, not a transient fault: fail fast instead of retrying
		// until the 24h timeout.
		return nil, temporal.NewNonRetryableApplicationError(
			"staging directory is not configured (set TAPE_STAGING_DIR on the data worker)",
			"prepare-misconfigured",
			nil,
		)
	}

	stagingDir := filepath.Join(a.stagingRoot, activity.GetInfo(ctx).WorkflowExecution.RunID)

	// Heartbeat so a data-worker restart is caught within activityHeartbeatTimeout
	// rather than the 24h StartToClose.
	var staged []StagedArchive

	progress := newPrepareProgressTracker(input.Archives, time.Now)

	record := func() {
		snapshot := progress.snapshot()
		activity.RecordHeartbeat(ctx, snapshot)
		logPrepareProgress(ctx, snapshot)
	}

	err := runWithHeartbeat(ctx, activityHeartbeatInterval, record, func() error {
		var err error

		staged, err = a.prepare(ctx, stagingDir, input, progress)

		return err
	})

	return staged, liftNonRetryable(err)
}

// liftNonRetryable re-raises a wrapped non-retryable ApplicationError at the
// activity boundary: Temporal only inspects the top-level error, so fmt.Errorf
// wrapping would otherwise make it retryable.
func liftNonRetryable(err error) error {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || !appErr.NonRetryable() || err == error(appErr) {
		return err
	}

	return temporal.NewNonRetryableApplicationError(err.Error(), appErr.Type(), err)
}

// logPrepareProgress inlines the numbers into the message because the web log
// panel shows only message text (pkg/runsapi.LogLine); the attributes remain for
// VictoriaLogs.
func logPrepareProgress(ctx context.Context, progress PrepareProgress) {
	attrs := []any{
		"completedArchives", progress.CompletedArchives,
		"totalArchives", progress.TotalArchives,
		"sourceBytesRead", progress.SourceBytesRead,
		"totalSourceBytes", progress.TotalSourceBytes,
		"overallPercent", progress.OverallPercent,
	}

	if progress.CurrentStage != "" {
		attrs = append(attrs,
			"currentSourceIndex", progress.CurrentSourceIndex,
			"currentLabel", progress.CurrentLabel,
			"currentStage", progress.CurrentStage,
			"currentArchivePercent", progress.CurrentArchivePercent,
		)
	}

	if progress.EstimatedTimeRemaining > 0 {
		attrs = append(attrs, "eta", progress.EstimatedTimeRemaining.String())
	}

	slog.InfoContext(ctx, prepareProgressMessage(progress), attrs...)
}

func prepareProgressMessage(progress PrepareProgress) string {
	msg := fmt.Sprintf("prepare: progress %.1f%% overall (%d/%d archives staged",
		progress.OverallPercent, progress.CompletedArchives, progress.TotalArchives)

	switch progress.CurrentStage {
	case prepareStageStreaming:
		msg += fmt.Sprintf(", streaming %q at %.1f%%", progress.CurrentLabel, progress.CurrentArchivePercent)
	case prepareStageChecksumming:
		msg += fmt.Sprintf(", checksumming %q slice %d/%d", progress.CurrentLabel,
			progress.ChecksummedSlices, progress.TotalSlices)
	}

	msg += ")"

	if progress.EstimatedTimeRemaining > 0 {
		msg += ", eta " + progress.EstimatedTimeRemaining.String()
	}

	return msg
}

// prepare stages every archive under stagingDir without needing an activity
// context. progress may be nil.
func (a *PrepareActivities) prepare(ctx context.Context, stagingDir string, input PrepareInput, progress *prepareProgressTracker) ([]StagedArchive, error) {
	recipients := input.Config.Encryption.Recipients
	sliceSize := input.SliceSizeBytes

	staged := make([]StagedArchive, 0, len(input.Archives))

	slog.InfoContext(ctx, "prepare: staging resolved archives to disk", "archives", len(input.Archives), "stagingDir", stagingDir)

	var totalBytes int64

	for _, resolvedArchive := range input.Archives {
		progress.startArchive(resolvedArchive.SourceIndex, resolvedArchive.Label, resolvedArchive.LogicalBytes)

		slog.InfoContext(ctx, "prepare: staging archive",
			"sourceIndex", resolvedArchive.SourceIndex, "label", resolvedArchive.Label,
			"compressed", resolvedArchive.Compression, "sourceBytes", resolvedArchive.LogicalBytes)

		result, err := a.stageArchive(ctx, stagingDir, resolvedArchive, recipients, sliceSize, progress)
		if err != nil {
			return nil, fmt.Errorf("prepare sources[%d]: %w", resolvedArchive.SourceIndex, err)
		}

		staged = append(staged, result)
		totalBytes += result.SizeBytes

		progress.finishArchive()

		slog.InfoContext(ctx, "prepare: staged archive",
			"sourceIndex", resolvedArchive.SourceIndex, "label", resolvedArchive.Label,
			"compressed", resolvedArchive.Compression, "slices", len(result.Slices), "bytes", result.SizeBytes)
	}

	slog.InfoContext(ctx, "prepare: staged all archives to disk", "archives", len(staged), "totalBytes", totalBytes)

	return staged, nil
}

// stageArchive runs one archive's pipeline into its staging directory, then
// measures and checksums each slice.
func (a *PrepareActivities) stageArchive(ctx context.Context, stagingDir string, resolvedArchive ResolvedArchive, recipients []string, sliceSize int64, progress *prepareProgressTracker) (StagedArchive, error) {
	archiveDir := filepath.Join(stagingDir, fmt.Sprintf("%03d", resolvedArchive.SourceIndex))
	if err := os.MkdirAll(archiveDir, stagingDirPerm); err != nil {
		return StagedArchive{}, fmt.Errorf("create staging directory %q: %w", archiveDir, err)
	}

	members, err := a.resolveMembers(ctx, resolvedArchive)
	if err != nil {
		return StagedArchive{}, err
	}

	slicePaths, err := runPipeline(ctx, archiveDir, members, resolvedArchive.Compression, recipients, sliceSize, progress)
	if err != nil {
		return StagedArchive{}, err
	}

	slices := make([]StagedSlice, 0, len(slicePaths))

	var total int64

	progress.startChecksums(len(slicePaths))

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

		progress.finishSlice()
	}

	return StagedArchive{SourceIndex: resolvedArchive.SourceIndex, Slices: slices, SizeBytes: total}, nil
}

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

// runPipeline streams tar → optional zstd → age → split through io.Pipes, so
// only the final slices touch disk. It returns the slice paths in order.
func runPipeline(ctx context.Context, archiveDir string, members []archive.Member, compress bool, recipients []string, sliceSize int64, progress *prepareProgressTracker) ([]string, error) {
	// Otherwise this surfaces only as a broken-pipe error from upstream stages.
	if sliceSize <= 0 {
		return nil, fmt.Errorf("slice size must be positive, got %d", sliceSize)
	}

	group, groupCtx := errgroup.WithContext(ctx)

	tarReader := pipeStage(group, nil, func(w io.Writer) error {
		return tarMembers(groupCtx, progress.countingWriter(w), members)
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

	// Unblock the encrypt stage if Split stopped reading early.
	encryptReader.CloseWithError(splitErr)

	if waitErr := group.Wait(); waitErr != nil {
		return nil, waitErr
	}

	if splitErr != nil {
		return nil, fmt.Errorf("split archive: %w", splitErr)
	}

	return slicePaths, nil
}

// pipeStage runs produce into a fresh pipe and returns its read end. It also
// closes input with produce's error so a failure unblocks the upstream producer
// instead of deadlocking it on a full pipe.
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

// tarMembers puts a single member at the archive root (SPEC §4.3 phase 2) and a
// group one subdirectory per member (SPEC §5).
func tarMembers(ctx context.Context, w io.Writer, members []archive.Member) error {
	if len(members) == 1 {
		return archive.Tar(ctx, w, members[0].Dir)
	}

	return archive.TarMembers(ctx, w, members)
}

// datasetAndSnapshot splits a snapshot into dataset and short snapshot name; a
// raw path with no "@" is a dataset with an empty snapshot name.
func datasetAndSnapshot(snapshot ResolvedSnapshot) (dataset, snapName string) {
	if snapshot.Dataset != "" {
		return snapshot.Dataset, snapshot.SnapshotName
	}

	if before, after, found := strings.Cut(snapshot.ZFSPath, "@"); found {
		return before, after
	}

	return snapshot.ZFSPath, ""
}

// memberName is a group member's subdirectory: the PVC name, else the dataset's
// last path component.
func memberName(snapshot ResolvedSnapshot) string {
	if snapshot.PVC != "" {
		return snapshot.PVC
	}

	dataset, _ := datasetAndSnapshot(snapshot)

	return filepath.Base(dataset)
}

// preparePhase runs the data-side Prepare activity and stores the staged
// archives for Pack. A failure aborts the run before any tape is touched.
func preparePhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: prepareTimeout,
		HeartbeatTimeout:    activityHeartbeatTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: prepareMaxAttempts},
	})

	var activities *PrepareActivities

	input := PrepareInput{Config: cfg, Archives: state.resolved, SliceSizeBytes: state.sliceSizeBytes}

	var staged []StagedArchive
	if err := workflow.ExecuteActivity(dataCtx, activities.PrepareArchives, input).Get(dataCtx, &staged); err != nil {
		return err
	}

	state.staged = staged

	return nil
}
