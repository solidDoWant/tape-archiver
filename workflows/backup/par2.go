package backup

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/checksum"
	"github.com/solidDoWant/tape-archiver/pkg/par2"
)

// The Generate PAR2 phase (SPEC §4.3 phase 4) stages a per-archive PAR2 recovery
// set beside each archive's slices. Fixed mode uses the configured percentage;
// fill-to-capacity raises it uniformly per tape to consume that tape's remaining
// capacity (hence Pack and Generate PAR2 are paired). Capping at 100% keeps the
// recovery block count bounded (SPEC §8).

const (
	// generatePAR2Timeout is generous: parity is computed over the full staged
	// tree, potentially terabytes.
	generatePAR2Timeout = 24 * time.Hour

	// generatePAR2MaxAttempts bounds retries so a deterministic failure fails the
	// run; a heartbeat timeout from a worker restart is still retried (#385).
	generatePAR2MaxAttempts = 3

	// par2SetName is the index file name; par2 writes "archive.vol*.par2" beside it.
	par2SetName = "archive.par2"

	// par2GlobPattern matches the index and volumes but never the slices
	// ("archive.NNN").
	par2GlobPattern = "archive*.par2"
)

// GeneratePAR2Activities hosts the data-side Generate PAR2 activity.
type GeneratePAR2Activities struct{}

func newGeneratePAR2Activities() *GeneratePAR2Activities { return &GeneratePAR2Activities{} }

// GeneratePAR2Input is the Generate PAR2 activity payload.
type GeneratePAR2Input struct {
	Config   config.Config
	Plan     TapePlan
	Archives []StagedArchive
}

// PAR2Progress is the Generate PAR2 heartbeat payload and progress-log snapshot,
// byte-weighted across archives using par2's own per-archive completion.
type PAR2Progress struct {
	TotalArchives     int `json:"totalArchives"`
	CompletedArchives int `json:"completedArchives"`

	CurrentSourceIndex       int     `json:"currentSourceIndex"`
	CurrentRedundancyPercent int     `json:"currentRedundancyPercent"`
	CurrentArchivePercent    float64 `json:"currentArchivePercent"`

	// EstimatedTimeRemaining projects the rate over the last etaWindow; zero
	// until there is progress to estimate from, or while progress is stalled.
	OverallPercent         float64       `json:"overallPercent"`
	EstimatedTimeRemaining time.Duration `json:"estimatedTimeRemaining"`
}

// par2ProgressTracker is advanced by the work goroutine and snapshotted by the
// heartbeat goroutine. Mutating methods are nil-safe (the test path).
type par2ProgressTracker struct {
	// Set at construction and read without the lock.
	now           func() time.Time
	totalArchives int
	totalBytes    int64

	mu             sync.Mutex
	eta            *etaEstimator
	completedCount int
	completedBytes int64
	current        par2CurrentArchive
}

type par2CurrentArchive struct {
	sourceIndex       int
	redundancyPercent int
	dataBytes         int64
	fraction          float64
}

func newPAR2ProgressTracker(archives []StagedArchive, now func() time.Time) *par2ProgressTracker {
	var totalBytes int64
	for _, archive := range archives {
		totalBytes += archive.SizeBytes
	}

	return &par2ProgressTracker{
		now:           now,
		eta:           newETAEstimator(now(), etaWindow),
		totalArchives: len(archives),
		totalBytes:    totalBytes,
	}
}

func (p *par2ProgressTracker) startArchive(sourceIndex, redundancyPercent int, dataBytes int64) {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current = par2CurrentArchive{
		sourceIndex:       sourceIndex,
		redundancyPercent: redundancyPercent,
		dataBytes:         dataBytes,
	}
}

// updateArchiveProgress is the par2.WithProgress callback; fraction is clamped to
// [0, 1].
func (p *par2ProgressTracker) updateArchiveProgress(fraction float64) {
	if p == nil {
		return
	}

	fraction = math.Min(math.Max(fraction, 0), 1)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current.fraction = fraction
}

func (p *par2ProgressTracker) finishArchive() {
	if p == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.completedBytes += p.current.dataBytes
	p.completedCount++
	p.current = par2CurrentArchive{}
}

func (p *par2ProgressTracker) snapshot() PAR2Progress {
	p.mu.Lock()
	defer p.mu.Unlock()

	doneBytes := p.completedBytes + int64(float64(p.current.dataBytes)*p.current.fraction)

	progress := PAR2Progress{
		TotalArchives:            p.totalArchives,
		CompletedArchives:        p.completedCount,
		CurrentSourceIndex:       p.current.sourceIndex,
		CurrentRedundancyPercent: p.current.redundancyPercent,
		CurrentArchivePercent:    roundPercent(p.current.fraction * 100),
	}

	if p.totalBytes > 0 {
		progress.OverallPercent = roundPercent(float64(doneBytes) / float64(p.totalBytes) * 100)

		progress.EstimatedTimeRemaining = p.eta.estimate(p.now(), doneBytes, p.totalBytes)
	}

	return progress
}

// roundPercent rounds to one decimal place, matching par2's 0.1% granularity.
func roundPercent(percent float64) float64 {
	return math.Round(percent*10) / 10
}

// GeneratePAR2 generates a recovery set for every staged archive (SPEC §4.3
// phase 4), returning one PAR2Set per input in order.
func (a *GeneratePAR2Activities) GeneratePAR2(ctx context.Context, input GeneratePAR2Input) ([]PAR2Set, error) {
	// Heartbeat so a data-worker restart is caught within activityHeartbeatTimeout
	// rather than the 24h StartToClose.
	var sets []PAR2Set

	progress := newPAR2ProgressTracker(input.Archives, time.Now)

	record := func() {
		snapshot := progress.snapshot()
		activity.RecordHeartbeat(ctx, snapshot)
		logPAR2Progress(ctx, snapshot)
	}

	err := runWithHeartbeat(ctx, activityHeartbeatInterval, record, func() error {
		var err error

		sets, err = generatePAR2(ctx, input.Config, input.Plan, input.Archives, progress)

		return err
	})

	return sets, err
}

// logPAR2Progress inlines the numbers into the message because the web log panel
// shows only message text (pkg/runsapi.LogLine).
func logPAR2Progress(ctx context.Context, progress PAR2Progress) {
	msg := fmt.Sprintf("par2: progress %.1f%% overall (archive %d/%d, %.1f%% of current)",
		progress.OverallPercent,
		progress.CompletedArchives,
		progress.TotalArchives,
		progress.CurrentArchivePercent,
	)

	attrs := []any{
		"completedArchives", progress.CompletedArchives,
		"totalArchives", progress.TotalArchives,
		"currentSourceIndex", progress.CurrentSourceIndex,
		"currentArchivePercent", progress.CurrentArchivePercent,
		"overallPercent", progress.OverallPercent,
	}

	if progress.EstimatedTimeRemaining > 0 {
		msg += ", eta " + progress.EstimatedTimeRemaining.String()
		attrs = append(attrs, "eta", progress.EstimatedTimeRemaining.String())
	}

	slog.InfoContext(ctx, msg, attrs...)
}

// generatePAR2 is the activity body without an activity context. progress may be
// nil.
func generatePAR2(ctx context.Context, cfg config.Config, plan TapePlan, staged []StagedArchive, progress *par2ProgressTracker) ([]PAR2Set, error) {
	if len(staged) == 0 {
		return nil, nil
	}

	percentByIndex, err := par2Percentages(cfg.Redundancy, plan)
	if err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "par2: generating recovery sets", "archives", len(staged))

	sets := make([]PAR2Set, 0, len(staged))

	for _, archive := range staged {
		percent, ok := percentByIndex[archive.SourceIndex]
		if !ok {
			return nil, fmt.Errorf("sources[%d] was staged but not placed on any tape by the Pack phase", archive.SourceIndex)
		}

		progress.startArchive(archive.SourceIndex, percent, archive.SizeBytes)

		slog.InfoContext(ctx, "par2: generating recovery set for archive",
			"sourceIndex", archive.SourceIndex, "redundancyPercent", percent,
			"slices", len(archive.Slices), "dataBytes", archive.SizeBytes)

		set, err := generateArchivePAR2(ctx, archive, percent, progress)
		if err != nil {
			return nil, fmt.Errorf("generate PAR2 for sources[%d]: %w", archive.SourceIndex, err)
		}

		sets = append(sets, set)

		progress.finishArchive()

		slog.InfoContext(ctx, "par2: generated recovery set for archive",
			"sourceIndex", set.SourceIndex, "redundancyPercent", set.RedundancyPercent,
			"recoveryFiles", len(set.Files), "bytes", par2SetBytes(set))
	}

	slog.InfoContext(ctx, "par2: generated recovery sets for all archives", "archives", len(sets))

	return sets, nil
}

func par2SetBytes(set PAR2Set) int64 {
	var total int64
	for _, file := range set.Files {
		total += file.SizeBytes
	}

	return total
}

// par2Percentages maps each placed archive's SourceIndex to its PAR2 redundancy
// percentage; fill-to-capacity computes one percentage per tape.
func par2Percentages(redundancy config.Redundancy, plan TapePlan) (map[int]int, error) {
	percentByIndex := make(map[int]int)

	switch {
	case redundancy.TargetPercentage != nil:
		percent := clampPercent(int(math.Round(*redundancy.TargetPercentage)))

		for _, tape := range plan.Tapes {
			for _, archive := range tape.Archives {
				percentByIndex[archive.SourceIndex] = percent
			}
		}
	case redundancy.FillToCapacity != nil:
		floor := clampPercent(int(math.Ceil(redundancy.FillToCapacity.Floor)))
		for _, tape := range plan.Tapes {
			percent := fillPercent(tape, floor)
			for _, archive := range tape.Archives {
				percentByIndex[archive.SourceIndex] = percent
			}
		}
	default:
		return nil, fmt.Errorf("redundancy: neither targetPercentage nor fillToCapacity is set")
	}

	return percentByIndex, nil
}

// fillPercent is the largest percentage in [floor, 100] whose modeled PAR2 output
// fits the tape's remaining capacity. It sizes against par2.MaxOutputBytes (the
// bound Pack reserves with), not data×percent/100, so the sets are guaranteed to
// fit and Verify cannot overflow after the compute (#148, SPEC §4.3 phases 3–4).
func fillPercent(tape PlannedTape, floor int) int {
	floor = clampPercent(floor)

	data := tape.DataBytes()
	if data <= 0 {
		return floor
	}

	slack := tape.UsableBytes - data
	if slack < 0 {
		slack = 0
	}

	// MaxOutputBytes is monotonic in the percentage, so stop at the first overflow.
	// Pack reserved the floor's bound, so the floor always fits.
	best := floor

	for percent := floor; percent <= maxPAR2Percent; percent++ {
		if tapePAR2Bound(tape, percent) > slack {
			break
		}

		best = percent
	}

	return best
}

// tapePAR2Bound sums par2.MaxOutputBytes across a tape's archives at percent.
func tapePAR2Bound(tape PlannedTape, percent int) int64 {
	var total int64
	for _, archive := range tape.Archives {
		total += par2.MaxOutputBytes(archive.DataBytes, archive.SliceCount, percent)
	}

	return total
}

// generateArchivePAR2 runs par2 over one archive's slices, then measures and
// checksums the recovery files. progress may be nil.
func generateArchivePAR2(ctx context.Context, archive StagedArchive, percent int, progress *par2ProgressTracker) (PAR2Set, error) {
	if len(archive.Slices) == 0 {
		return PAR2Set{}, fmt.Errorf("staged archive has no slices to protect")
	}

	// Written beside the slices so par2's basename references stay valid when read
	// back from tape.
	archiveDir := filepath.Dir(archive.Slices[0].Path)
	recoverySetPath := filepath.Join(archiveDir, par2SetName)

	dataFiles := make([]string, len(archive.Slices))
	for index, slice := range archive.Slices {
		dataFiles[index] = slice.Path
	}

	// par2 create refuses to overwrite existing recovery files, so a retry after a
	// partial attempt would fail deterministically. Purge with the same glob
	// stagePAR2Files reads, so no stale file can be recorded or shipped.
	if err := purgeStagedPAR2Files(archiveDir); err != nil {
		return PAR2Set{}, err
	}

	// Only with a tracker, so the test path keeps par2's -qq.
	var opts []par2.Option
	if progress != nil {
		opts = append(opts, par2.WithProgress(progress.updateArchiveProgress))
	}

	if err := par2.Generate(ctx, recoverySetPath, dataFiles, percent, opts...); err != nil {
		return PAR2Set{}, err
	}

	files, err := stagePAR2Files(archiveDir)
	if err != nil {
		return PAR2Set{}, err
	}

	return PAR2Set{SourceIndex: archive.SourceIndex, RedundancyPercent: percent, Files: files}, nil
}

// purgeStagedPAR2Files removes recovery files left by a prior partial attempt. No
// matches is not an error.
func purgeStagedPAR2Files(archiveDir string) error {
	matches, err := filepath.Glob(filepath.Join(archiveDir, par2GlobPattern))
	if err != nil {
		return fmt.Errorf("list leftover PAR2 files in %q: %w", archiveDir, err)
	}

	for _, path := range matches {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove leftover PAR2 file %q: %w", path, err)
		}
	}

	return nil
}

// stagePAR2Files measures and checksums every recovery file in archiveDir, sorted
// by name. par2 names the files itself, so they are discovered by glob.
func stagePAR2Files(archiveDir string) ([]StagedSlice, error) {
	matches, err := filepath.Glob(filepath.Join(archiveDir, par2GlobPattern))
	if err != nil {
		return nil, fmt.Errorf("list PAR2 files in %q: %w", archiveDir, err)
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("par2 produced no recovery files in %q", archiveDir)
	}

	sort.Strings(matches)

	files := make([]StagedSlice, 0, len(matches))

	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("measure PAR2 file %q: %w", path, err)
		}

		digest, err := checksum.SHA256File(path)
		if err != nil {
			return nil, fmt.Errorf("checksum PAR2 file %q: %w", path, err)
		}

		files = append(files, StagedSlice{Path: path, SHA256: digest, SizeBytes: info.Size()})
	}

	return files, nil
}

// generatePAR2Phase runs the data-side activity and stores the recovery sets for
// Verify and Write. A failure aborts the run before any tape is touched.
func generatePAR2Phase(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: generatePAR2Timeout,
		HeartbeatTimeout:    activityHeartbeatTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: generatePAR2MaxAttempts},
	})

	var activities *GeneratePAR2Activities

	input := GeneratePAR2Input{Config: cfg, Plan: state.plan, Archives: state.staged}

	var sets []PAR2Set
	if err := workflow.ExecuteActivity(dataCtx, activities.GeneratePAR2, input).Get(dataCtx, &sets); err != nil {
		return err
	}

	state.par2 = sets

	return nil
}
