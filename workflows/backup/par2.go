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
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/checksum"
	"github.com/solidDoWant/tape-archiver/pkg/par2"
)

// The Generate PAR2 phase (SPEC §4.3 phase 4) produces each archive's per-archive
// PAR2 recovery set, staged to disk alongside its slices and checksummed. The
// redundancy percentage is chosen per the run's PAR2 mode:
//
//   - Fixed-percentage mode sizes every recovery set to the configured target.
//   - Fill-to-capacity mode raises the percentage uniformly across the archives on
//     each tape to consume that tape's remaining capacity (from the Pack plan)
//     down to the configured floor — this is why Pack and Generate PAR2 are paired.
//
// The PAR2 block size that keeps the recovery block count within PAR2's
// 32,768-block hard limit is computed inside pkg/par2 from the data size; capping
// the percentage at 100 keeps the recovery block count bounded too (SPEC §8). The
// recovery files land beside the slices on the data worker, where the bytes live
// (SPEC §4.1), so this phase runs on the data queue.

const (
	// generatePAR2Timeout bounds the whole Generate PAR2 activity. par2cmdline-turbo
	// computes parity over the full staged tree — potentially terabytes — so the
	// bound is deliberately generous, like Prepare's.
	generatePAR2Timeout = 24 * time.Hour

	// par2SetName is the recovery set's base file name within each archive's
	// staging directory; par2 writes the index here and its recovery volumes as
	// "archive.vol*.par2" beside it (matching pkg/par2's convention).
	par2SetName = "archive.par2"

	// par2GlobPattern matches the recovery set's files — the index plus every
	// volume file — without matching the archive's slices (named "archive.NNN",
	// which never end in ".par2").
	par2GlobPattern = "archive*.par2"
)

// GeneratePAR2Activities hosts the data-side Generate PAR2 activity. It carries no
// dependencies: parity generation shells out through pkg/par2 to the bundled par2
// binary, and the slices to protect are located by the absolute paths the Prepare
// phase recorded.
type GeneratePAR2Activities struct{}

// newGeneratePAR2Activities returns the data-side Generate PAR2 activity.
func newGeneratePAR2Activities() *GeneratePAR2Activities { return &GeneratePAR2Activities{} }

// GeneratePAR2Input is the payload for the Generate PAR2 activity: the run config
// (for the PAR2 mode), the Pack plan (for each archive's tape and that tape's
// remaining capacity), and the staged work list whose slices are protected.
type GeneratePAR2Input struct {
	Config   config.Config
	Plan     TapePlan
	Archives []StagedArchive
}

// PAR2Progress is the Generate PAR2 activity's progress snapshot: it is the
// heartbeat payload Temporal surfaces while the phase runs (and hands a retried
// attempt via activity.GetHeartbeatDetails), and it is what the periodic progress
// log reports. par2 crunches archives one at a time and a single archive can take
// hours over terabytes, so the snapshot combines the per-archive count with the
// in-flight archive's own completion fraction (parsed from par2) into a
// byte-weighted overall percentage and a projected time to finish.
type PAR2Progress struct {
	// TotalArchives is how many archives the phase will generate a recovery set
	// for; CompletedArchives is how many have finished.
	TotalArchives     int `json:"totalArchives"`
	CompletedArchives int `json:"completedArchives"`

	// CurrentSourceIndex and CurrentRedundancyPercent identify the archive whose
	// parity is being computed right now — the slow par2 call — and
	// CurrentArchivePercent is that archive's own completion (0–100), from par2.
	CurrentSourceIndex       int     `json:"currentSourceIndex"`
	CurrentRedundancyPercent int     `json:"currentRedundancyPercent"`
	CurrentArchivePercent    float64 `json:"currentArchivePercent"`

	// OverallPercent is byte-weighted progress across every archive (0–100), and
	// EstimatedTimeRemaining projects the time left from the byte rate observed so
	// far. EstimatedTimeRemaining is zero until there is enough progress to
	// estimate (and near zero as the phase finishes).
	OverallPercent         float64       `json:"overallPercent"`
	EstimatedTimeRemaining time.Duration `json:"estimatedTimeRemaining"`
}

// par2ProgressTracker holds the live progress the Generate PAR2 heartbeat and log
// report. The work goroutine advances it — per archive, and (via par2's progress
// callback) within the archive in flight — while the heartbeat goroutine snapshots
// it, so its methods take a lock. The mutating methods are nil-safe: a nil tracker
// (the test path, which runs generatePAR2 without a heartbeat) records nothing.
type par2ProgressTracker struct {
	// now and startedAt drive the ETA; totalArchives and totalBytes are the fixed
	// denominators. All are set at construction and read without the lock.
	now           func() time.Time
	startedAt     time.Time
	totalArchives int
	totalBytes    int64

	mu             sync.Mutex
	completedCount int
	completedBytes int64
	current        par2CurrentArchive
}

// par2CurrentArchive is the tracker's view of the archive whose parity is being
// computed: its identity, its data size (for byte-weighting), and its own
// completion fraction as par2 reports it.
type par2CurrentArchive struct {
	sourceIndex       int
	redundancyPercent int
	dataBytes         int64
	fraction          float64
}

// newPAR2ProgressTracker returns a tracker for a phase generating recovery sets
// for the given archives, using now as its clock (injected so the ETA is
// testable). startedAt is stamped now so the ETA measures from phase start.
func newPAR2ProgressTracker(archives []StagedArchive, now func() time.Time) *par2ProgressTracker {
	var totalBytes int64
	for _, archive := range archives {
		totalBytes += archive.SizeBytes
	}

	return &par2ProgressTracker{
		now:           now,
		startedAt:     now(),
		totalArchives: len(archives),
		totalBytes:    totalBytes,
	}
}

// startArchive records that generation of the given archive has begun.
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

// updateArchiveProgress records the in-flight archive's completion fraction (from
// par2), clamped to [0, 1]. It is the par2.WithProgress callback.
func (p *par2ProgressTracker) updateArchiveProgress(fraction float64) {
	if p == nil {
		return
	}

	fraction = math.Min(math.Max(fraction, 0), 1)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current.fraction = fraction
}

// finishArchive records that the archive in flight has completed: its bytes fold
// into the completed total and the in-flight slot clears, so no bytes are
// double-counted between archives.
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

// snapshot computes the current PAR2Progress: the archive counts, the in-flight
// archive, the byte-weighted overall percentage, and an ETA projected from the
// byte rate observed since the phase started.
func (p *par2ProgressTracker) snapshot() PAR2Progress {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Bytes done so far: every completed archive in full, plus the in-flight
	// archive weighted by its own reported fraction.
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

// roundPercent rounds a percentage to one decimal place, matching par2's own 0.1%
// progress granularity and keeping the payload and log tidy.
func roundPercent(percent float64) float64 {
	return math.Round(percent*10) / 10
}

// GeneratePAR2 generates a per-archive PAR2 recovery set for every staged archive
// (SPEC §4.3 phase 4), returning a PAR2Set per input archive in the same order,
// each with its recovery files' paths, sizes, and SHA-256 checksums.
func (a *GeneratePAR2Activities) GeneratePAR2(ctx context.Context, input GeneratePAR2Input) ([]PAR2Set, error) {
	// Emit a liveness heartbeat while generating parity so a data-worker restart
	// mid-phase is caught within activityHeartbeatTimeout rather than the 24h
	// StartToClose. Each tick also records the progress snapshot as the heartbeat
	// payload and logs it, so a long archive is neither silent in the logs nor
	// opaque in the Temporal UI — with a byte-weighted percentage and ETA.
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

// logPAR2Progress emits one periodic progress line for the Generate PAR2 phase.
// The ETA is included only once it is estimable, so the line does not imply a
// bogus "0s remaining" before the first bytes are done.
func logPAR2Progress(ctx context.Context, progress PAR2Progress) {
	attrs := []any{
		"completedArchives", progress.CompletedArchives,
		"totalArchives", progress.TotalArchives,
		"currentSourceIndex", progress.CurrentSourceIndex,
		"currentArchivePercent", progress.CurrentArchivePercent,
		"overallPercent", progress.OverallPercent,
	}

	if progress.EstimatedTimeRemaining > 0 {
		attrs = append(attrs, "eta", progress.EstimatedTimeRemaining.String())
	}

	slog.InfoContext(ctx, "par2: progress", attrs...)
}

// generatePAR2 is the body of the Generate PAR2 activity, split out so it can be
// exercised without an activity context. progress may be nil (the test path); when
// set it is advanced per archive so the caller's heartbeat can report which archive
// is in flight.
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

// par2SetBytes sums the on-disk size of a recovery set's staged files, for the
// per-archive progress log.
func par2SetBytes(set PAR2Set) int64 {
	var total int64
	for _, file := range set.Files {
		total += file.SizeBytes
	}

	return total
}

// par2Percentages maps each placed archive's SourceIndex to the integer PAR2
// redundancy percentage its recovery set is generated at. Fixed mode uses the
// configured target for every archive; fill-to-capacity mode computes one
// percentage per tape and applies it uniformly to that tape's archives.
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

// fillPercent is the uniform PAR2 redundancy for one tape in fill-to-capacity
// mode: the largest integer percentage, from the floor up to 100, at which the
// tape's archives' modeled PAR2 output still fits the tape's remaining capacity.
//
// It grows every archive's recovery set to consume the slack between the tape's
// data and its usable capacity, never below the floor. Crucially it sizes against
// par2.MaxOutputBytes — the same conservative upper bound Pack reserves with — not
// the naive data×percent/100: since real par2 output ≤ that bound ≤ the remaining
// slack, the recovery sets are guaranteed to fit and Verify cannot overflow after
// the PAR2 compute (issue #148, SPEC §4.3 phases 3–4).
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

	// MaxOutputBytes is monotonically non-decreasing in the percentage, so scan up
	// from the floor and stop at the first percentage that overflows the slack. Pack
	// reserved the floor's bound within capacity, so the floor itself always fits.
	best := floor

	for percent := floor; percent <= maxPAR2Percent; percent++ {
		if tapePAR2Bound(tape, percent) > slack {
			break
		}

		best = percent
	}

	return best
}

// tapePAR2Bound is the summed conservative PAR2 upper bound (par2.MaxOutputBytes)
// across a tape's archives at the given redundancy percentage — the modeled parity
// footprint fill-to-capacity sizing keeps within the tape's remaining capacity.
func tapePAR2Bound(tape PlannedTape, percent int) int64 {
	var total int64
	for _, archive := range tape.Archives {
		total += par2.MaxOutputBytes(archive.DataBytes, archive.SliceCount, percent)
	}

	return total
}

// generateArchivePAR2 generates and stages one archive's PAR2 recovery set at the
// given percentage: it runs par2 over the archive's slices, then measures and
// checksums every recovery file the run produced. When progress is non-nil the
// archive's par2 completion fraction is fed into it (for the phase's heartbeat and
// log); a nil tracker (the test path) generates quietly.
func generateArchivePAR2(ctx context.Context, archive StagedArchive, percent int, progress *par2ProgressTracker) (PAR2Set, error) {
	if len(archive.Slices) == 0 {
		return PAR2Set{}, fmt.Errorf("staged archive has no slices to protect")
	}

	// The slices share one staging directory (Prepare stages each archive in its
	// own); the recovery set is written there too, so par2's basename references
	// stay valid when the files are read back from tape (pkg/par2).
	archiveDir := filepath.Dir(archive.Slices[0].Path)
	recoverySetPath := filepath.Join(archiveDir, par2SetName)

	dataFiles := make([]string, len(archive.Slices))
	for index, slice := range archive.Slices {
		dataFiles[index] = slice.Path
	}

	// par2 create exits non-zero rather than overwriting when recovery files
	// already exist, so a retry after a partial attempt (the worker-restart case
	// the liveness heartbeat exists for) would fail deterministically. Purge any
	// leftover recovery files first so generation is self-idempotent. The purge set
	// is exactly the glob stagePAR2Files reads back, so no stale file from an
	// earlier attempt can survive to be recorded or shipped.
	if err := purgeStagedPAR2Files(archiveDir); err != nil {
		return PAR2Set{}, err
	}

	// Feed par2's live completion fraction into the tracker so the phase's
	// heartbeat and log can report within-archive progress and an ETA. Only when a
	// tracker is present, so the test path stays fully quiet (keeping par2's -qq).
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

// purgeStagedPAR2Files removes every PAR2 recovery file in archiveDir (the index
// plus any volume files) left by a prior partial attempt. It globs the same
// par2GlobPattern stagePAR2Files reads back — matching only "archive*.par2", never
// the archive's slices ("archive.NNN") — so generation starts from a clean recovery
// set on every retry. A missing directory or no matches is not an error.
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

// stagePAR2Files measures and checksums every PAR2 recovery file in archiveDir,
// returning them in sorted name order. It is how the phase records its staged,
// checksummed output (SPEC §4.3 phase 4): par2 names the files itself, so they are
// discovered by glob rather than predicted.
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

// generatePAR2Phase orchestrates the Generate PAR2 phase (SPEC §4.3 phase 4): it
// runs the data-side activity over the staged work list and the Pack plan, and
// stores the recovery sets in runState for the Verify and Write phases. A failure
// aborts the run here, before any tape is touched.
func generatePAR2Phase(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: generatePAR2Timeout,
		HeartbeatTimeout:    activityHeartbeatTimeout,
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
