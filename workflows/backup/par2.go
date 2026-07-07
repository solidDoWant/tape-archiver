package backup

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

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

// GeneratePAR2 generates a per-archive PAR2 recovery set for every staged archive
// (SPEC §4.3 phase 4), returning a PAR2Set per input archive in the same order,
// each with its recovery files' paths, sizes, and SHA-256 checksums.
func (a *GeneratePAR2Activities) GeneratePAR2(ctx context.Context, input GeneratePAR2Input) ([]PAR2Set, error) {
	// Emit a liveness heartbeat while generating parity so a data-worker restart
	// mid-phase is caught within activityHeartbeatTimeout rather than the 24h
	// StartToClose.
	var sets []PAR2Set

	err := withActivityHeartbeat(ctx, func() error {
		var err error

		sets, err = generatePAR2(ctx, input.Config, input.Plan, input.Archives)

		return err
	})

	return sets, err
}

// generatePAR2 is the body of the Generate PAR2 activity, split out so it can be
// exercised without an activity context.
func generatePAR2(ctx context.Context, cfg config.Config, plan TapePlan, staged []StagedArchive) ([]PAR2Set, error) {
	if len(staged) == 0 {
		return nil, nil
	}

	percentByIndex, err := par2Percentages(cfg.Redundancy, plan)
	if err != nil {
		return nil, err
	}

	sets := make([]PAR2Set, 0, len(staged))

	for _, archive := range staged {
		percent, ok := percentByIndex[archive.SourceIndex]
		if !ok {
			return nil, fmt.Errorf("sources[%d] was staged but not placed on any tape by the Pack phase", archive.SourceIndex)
		}

		set, err := generateArchivePAR2(ctx, archive, percent)
		if err != nil {
			return nil, fmt.Errorf("generate PAR2 for sources[%d]: %w", archive.SourceIndex, err)
		}

		sets = append(sets, set)
	}

	return sets, nil
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
// checksums every recovery file the run produced.
func generateArchivePAR2(ctx context.Context, archive StagedArchive, percent int) (PAR2Set, error) {
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

	if err := par2.Generate(ctx, recoverySetPath, dataFiles, percent); err != nil {
		return PAR2Set{}, err
	}

	files, err := stagePAR2Files(archiveDir)
	if err != nil {
		return PAR2Set{}, err
	}

	return PAR2Set{SourceIndex: archive.SourceIndex, RedundancyPercent: percent, Files: files}, nil
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
