package backup

import (
	"context"
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

	// prepareStageStreaming and prepareStageChecksumming are the two stages an
	// archive passes through, reported in the phase's progress so a long archive is
	// distinguishable from a stalled one. Streaming is the tar → zstd → age → split
	// pipeline; checksumming is the SHA-256 pass over the slices it produced.
	prepareStageStreaming    = "streaming"
	prepareStageChecksumming = "checksumming"
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
		// A raw dataset source archives the dataset's live mountpoint directly.
		// An unmounted dataset's mountpoint can survive as an ordinary "shadow"
		// directory, so archiving it would silently certify empty or stale
		// contents (issue #135). Reading the mounted property in Prepare fails
		// the run here, before any tape is written (SPEC §4.3).
		mounted, err := zfs.Mounted(ctx, dataset)
		if err != nil {
			return "", fmt.Errorf("check mount state for %q: %w", dataset, err)
		}

		if !mounted {
			return "", fmt.Errorf("raw ZFS dataset %q is not mounted; refusing to archive its (shadow) mountpoint %q", dataset, mount)
		}

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
// recipients), the resolved work list to stage, and the slice size the Resolve
// phase derived for the run (DeriveSliceSize; issue #324).
type PrepareInput struct {
	Config         config.Config
	Archives       []ResolvedArchive
	SliceSizeBytes int64
}

// PrepareProgress is the Prepare activity's progress snapshot: it is the
// heartbeat payload Temporal surfaces while the phase runs (and hands a retried
// attempt via activity.GetHeartbeatDetails), and it is what the periodic progress
// log reports. Prepare streams archives one at a time and a single archive can
// take hours over terabytes, so the snapshot combines the per-archive count with
// the in-flight archive's own completion into a byte-weighted overall percentage
// and a projected time to finish.
//
// Progress is measured on the source side — the bytes the tar stage has read out
// of the snapshot trees, against the summed logicalreferenced Resolve recorded
// (ResolvedArchive.LogicalBytes). The staged output size is not knowable in
// advance (compression and encryption both change it), whereas the input is, so
// the source side is the only honest denominator.
type PrepareProgress struct {
	// TotalArchives is how many archives the phase will stage; CompletedArchives
	// is how many have been staged, checksummed, and measured.
	TotalArchives     int `json:"totalArchives"`
	CompletedArchives int `json:"completedArchives"`

	// CurrentSourceIndex and CurrentLabel identify the archive being staged right
	// now, CurrentStage is which of its two stages is running
	// (prepareStageStreaming or prepareStageChecksumming), and
	// CurrentArchivePercent is that archive's own source-byte completion (0–100).
	// All are zero-valued when no archive is in flight.
	CurrentSourceIndex    int     `json:"currentSourceIndex"`
	CurrentLabel          string  `json:"currentLabel"`
	CurrentStage          string  `json:"currentStage"`
	CurrentArchivePercent float64 `json:"currentArchivePercent"`

	// ChecksummedSlices and TotalSlices track the in-flight archive's checksum
	// pass, which reads back every slice the pipeline just wrote. Source bytes stop
	// moving during that pass, so this counter is what shows it is progressing.
	ChecksummedSlices int `json:"checksummedSlices"`
	TotalSlices       int `json:"totalSlices"`

	// SourceBytesRead and TotalSourceBytes are the raw counters OverallPercent is
	// derived from, reported so a run's throughput can be read off the log. A
	// completed archive counts its full source size; the in-flight one counts what
	// tar has read so far.
	SourceBytesRead  int64 `json:"sourceBytesRead"`
	TotalSourceBytes int64 `json:"totalSourceBytes"`

	// OverallPercent is byte-weighted progress across every archive (0–100), and
	// EstimatedTimeRemaining projects the time left from the byte rate observed so
	// far. EstimatedTimeRemaining is zero until there is enough progress to
	// estimate (and near zero as the phase finishes).
	OverallPercent         float64       `json:"overallPercent"`
	EstimatedTimeRemaining time.Duration `json:"estimatedTimeRemaining"`
}

// prepareProgressTracker holds the live progress the Prepare heartbeat and log
// report. The staging goroutines advance it — per archive, per source byte the
// tar stage emits, and per slice checksummed — while the heartbeat goroutine
// snapshots it, so its methods take a lock. The mutating methods are nil-safe: a
// nil tracker (the test path, which runs prepare without a heartbeat) records
// nothing.
type prepareProgressTracker struct {
	// now and startedAt drive the ETA; totalArchives and totalBytes are the fixed
	// denominators. All are set at construction and read without the lock.
	now           func() time.Time
	startedAt     time.Time
	totalArchives int
	totalBytes    int64

	mu             sync.Mutex
	completedCount int
	completedBytes int64
	current        prepareCurrentArchive
}

// prepareCurrentArchive is the tracker's view of the archive being staged: its
// identity, its source size (for byte-weighting), how much of that source the tar
// stage has read, and which stage it is in.
type prepareCurrentArchive struct {
	sourceIndex  int
	label        string
	logicalBytes int64
	readBytes    int64
	stage        string
	totalSlices  int
	doneSlices   int
}

// newPrepareProgressTracker returns a tracker for a phase staging the given
// archives, using now as its clock (injected so the ETA is testable). startedAt is
// stamped now so the ETA measures from phase start.
func newPrepareProgressTracker(archives []ResolvedArchive, now func() time.Time) *prepareProgressTracker {
	var totalBytes int64
	for _, resolvedArchive := range archives {
		totalBytes += resolvedArchive.LogicalBytes
	}

	return &prepareProgressTracker{
		now:           now,
		startedAt:     now(),
		totalArchives: len(archives),
		totalBytes:    totalBytes,
	}
}

// startArchive records that staging of the given archive has begun, in the
// streaming stage.
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

// addSourceBytes records source bytes the tar stage has emitted for the archive
// in flight. It is the counting writer's callback, so it is called per write and
// must stay cheap.
func (p *prepareProgressTracker) addSourceBytes(n int64) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current.readBytes += n
}

// startChecksums records that the archive in flight has finished streaming and is
// now being checksummed, over the given number of slices.
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

// finishSlice records one slice measured and checksummed.
func (p *prepareProgressTracker) finishSlice() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current.doneSlices++
}

// finishArchive records that the archive in flight has been fully staged: its
// source bytes fold into the completed total and the in-flight slot clears, so no
// bytes are double-counted between archives. The archive's full logical size is
// credited rather than the bytes actually read, so a tree that tars to slightly
// less than logicalreferenced (sparse files) still leaves the phase at 100%.
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

// countingWriter wraps w so every byte written through it is added to the
// in-flight archive's source-byte count. A nil tracker returns w untouched, so
// the test path streams without any counting overhead.
func (p *prepareProgressTracker) countingWriter(w io.Writer) io.Writer {
	if p == nil {
		return w
	}

	return &countingWriter{writer: w, count: p.addSourceBytes}
}

// snapshot computes the current PrepareProgress: the archive counts, the
// in-flight archive and its stage, the byte-weighted overall percentage, and an
// ETA projected from the byte rate observed since the phase started.
func (p *prepareProgressTracker) snapshot() PrepareProgress {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Bytes done so far: every completed archive in full, plus what the in-flight
	// archive has read. Clamped to that archive's size so tar's per-entry headers
	// and padding cannot push the phase past 100%.
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

		// Project the remaining time from the average byte rate so far. Needs some
		// progress and elapsed time to divide by; otherwise the ETA stays zero
		// ("not yet estimable").
		if elapsed := p.now().Sub(p.startedAt); elapsed > 0 && doneBytes > 0 {
			remaining := p.totalBytes - doneBytes
			eta := time.Duration(float64(remaining) / float64(doneBytes) * float64(elapsed))
			progress.EstimatedTimeRemaining = eta.Round(time.Second)
		}
	}

	return progress
}

// countingWriter is an io.Writer that reports the bytes it passes through to
// count. It wraps the Prepare pipeline's tar stage, which is where the phase's
// only knowable-in-advance quantity — source bytes — flows.
type countingWriter struct {
	writer io.Writer
	count  func(int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	written, err := c.writer.Write(p)

	// Count what was written even on a short or failing write, so the count never
	// runs ahead of the stream.
	c.count(int64(written))

	return written, err
}

// PrepareArchives stages every resolved archive to disk (SPEC §4.3 phase 2),
// returning a StagedArchive per input archive in the same order, each with its
// slice paths, per-slice SHA-256, and measured total size. Output is isolated
// under a per-run subdirectory of the staging root so concurrent or repeated runs
// never collide; the run id comes from the activity context.
func (a *PrepareActivities) PrepareArchives(ctx context.Context, input PrepareInput) ([]StagedArchive, error) {
	if a.stagingRoot == "" {
		// An unconfigured staging root is worker misconfiguration, not a transient
		// fault: every retry hits the same missing env var. Mark it non-retryable
		// so the run fails fast at the first attempt instead of retrying an
		// unrecoverable state under the default policy until the 24h timeout.
		return nil, temporal.NewNonRetryableApplicationError(
			"staging directory is not configured (set TAPE_STAGING_DIR on the data worker)",
			"prepare-misconfigured",
			nil,
		)
	}

	stagingDir := filepath.Join(a.stagingRoot, activity.GetInfo(ctx).WorkflowExecution.RunID)

	// Emit a liveness heartbeat while staging so a data-worker restart mid-Prepare
	// is caught within activityHeartbeatTimeout rather than the 24h StartToClose.
	// Each tick also records the progress snapshot as the heartbeat payload and
	// logs it, so a multi-hour archive is neither silent in the logs nor opaque in
	// the Temporal UI — with a byte-weighted percentage and ETA.
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

	return staged, err
}

// logPrepareProgress emits one periodic progress line for the Prepare phase. The
// ETA is included only once it is estimable, so the line does not imply a bogus
// "0s remaining" before the first bytes are staged.
//
// The numbers are inlined into the message, not left solely in the structured
// attributes: the web log panel projects only the message text (see
// pkg/runsapi.LogLine), so a constant "prepare: progress" message would read as
// blank progress there. The structured attributes are retained for VictoriaLogs
// and the Temporal heartbeat payload.
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

// prepareProgressMessage renders a progress snapshot as the log line's message
// text — the whole of what the web log panel shows, so every number an operator
// needs to see the phase moving lives here rather than in the attributes.
func prepareProgressMessage(progress PrepareProgress) string {
	msg := fmt.Sprintf("prepare: progress %.1f%% overall (%d/%d archives staged",
		progress.OverallPercent, progress.CompletedArchives, progress.TotalArchives)

	// The in-flight detail is omitted between archives, when there is none.
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

// prepare stages every archive under stagingDir. It is split from
// PrepareArchives so it can be exercised against a real directory without an
// activity context. progress may be nil (the test path); when set it is advanced
// per archive, per source byte, and per slice checksummed so the caller's
// heartbeat can report where the phase is.
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

// stageArchive runs the per-archive prepare pipeline and measures its output. It
// creates the archive's staging directory, streams tar → optional zstd → age →
// split into it, then stats and checksums each slice to produce the StagedArchive.
// progress may be nil; when set it is advanced through both stages.
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

	// The checksum pass reads every staged slice back, so it can run for a while
	// with no source bytes moving; report it as its own stage.
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
//
// progress may be nil; when set, the tar stage's output is counted through it so
// the phase can report how much of the source it has read.
func runPipeline(ctx context.Context, archiveDir string, members []archive.Member, compress bool, recipients []string, sliceSize int64, progress *prepareProgressTracker) ([]string, error) {
	// Validate up front: a non-positive slice size makes Split fail before it
	// reads, which would otherwise surface only as a broken-pipe error from the
	// upstream stages rather than this clear cause.
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
		HeartbeatTimeout:    activityHeartbeatTimeout,
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
