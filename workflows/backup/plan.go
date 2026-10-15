package backup

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/par2"
)

// The Pack phase (SPEC §4.3 phase 3) bin-packs archives onto tapes by their
// measured staged size, never the Resolve estimate. Each tape's data plus its
// minimum PAR2 reserve must fit native capacity less LTFS overhead; Generate PAR2
// grows parity into the remaining slack in fill mode (SPEC §4.3 phases 3–4).

const (
	packTimeout = 5 * time.Minute

	// ltfsOverheadFraction reserves native capacity for LTFS, dominated by
	// mkltfs's two-partition format (index partition plus wrap-granular guard):
	// ~1.4% on LTO-9, ~3% on LTO-6, up to ~5% on LTO-5. Erring high is safe — a
	// tape must never hit ENOSPC deep in the write window (SPEC §14) — and the
	// true figure cannot be measured at plan time or on mhvtl (#148).
	ltfsOverheadFraction = 0.05

	// par2cmdline rejects percentages outside [1, 100] (SPEC §8).
	minPAR2Percent = 1
	maxPAR2Percent = 100
)

// PackActivities hosts the control-side Pack activity.
type PackActivities struct{}

func newPackActivities() *PackActivities { return &PackActivities{} }

// PackInput is the Pack activity payload.
type PackInput struct {
	Config   config.Config
	Archives []StagedArchive
}

// Pack bin-packs the staged archives onto tapes (SPEC §4.3 phase 3).
func (a *PackActivities) Pack(ctx context.Context, input PackInput) (TapePlan, error) {
	plan, err := pack(input.Config, input.Archives)
	if err != nil {
		return TapePlan{}, err
	}

	slog.InfoContext(ctx, "pack: bin-packed staged archives onto tapes",
		"archives", len(input.Archives), "logicalTapes", len(plan.Tapes), "copies", plan.Copies,
		"physicalTapes", len(plan.Tapes)*plan.Copies)

	return plan, nil
}

func pack(cfg config.Config, staged []StagedArchive) (TapePlan, error) {
	if len(staged) == 0 {
		return TapePlan{Copies: cfg.Copies}, nil
	}

	copies := cfg.Copies

	if copies < 1 {
		return TapePlan{}, fmt.Errorf("copies must be at least 1, got %d", copies)
	}

	// Copies may exceed the drive count: they are written in successive drive-sets
	// (SPEC §4.3 phases 6–8).

	usable := usableCapacity(cfg.Library.TapeCapacityBytes)
	if usable <= 0 {
		return TapePlan{}, fmt.Errorf("tape capacity %d bytes leaves no usable space after LTFS overhead", cfg.Library.TapeCapacityBytes)
	}

	reservePercent := par2ReservePercent(cfg.Redundancy)

	items := make([]PlannedArchive, len(staged))

	for index, archive := range staged {
		item := PlannedArchive{
			SourceIndex:       archive.SourceIndex,
			DataBytes:         archive.SizeBytes,
			SliceCount:        len(archive.Slices),
			PAR2ReservedBytes: reservedBytes(archive.SizeBytes, len(archive.Slices), reservePercent),
		}

		// The measured size can exceed the Resolve estimate.
		if footprint := item.Footprint(); footprint > usable {
			return TapePlan{}, fmt.Errorf(
				"sources[%d] measured footprint %d bytes (data %d + PAR2 %d) exceeds one tape's usable capacity %d bytes",
				archive.SourceIndex, footprint, item.DataBytes, item.PAR2ReservedBytes, usable,
			)
		}

		items[index] = item
	}

	// First-fit-decreasing.
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Footprint() > items[j].Footprint()
	})

	var tapes []PlannedTape

	for _, item := range items {
		placed := false

		for index := range tapes {
			if tapes[index].PlannedBytes()+item.Footprint() <= usable {
				tapes[index].Archives = append(tapes[index].Archives, item)
				placed = true

				break
			}
		}

		if !placed {
			tapes = append(tapes, PlannedTape{Archives: []PlannedArchive{item}, UsableBytes: usable})
		}
	}

	for index := range tapes {
		sort.Slice(tapes[index].Archives, func(i, j int) bool {
			return tapes[index].Archives[i].SourceIndex < tapes[index].Archives[j].SourceIndex
		})
	}

	return TapePlan{Copies: copies, Tapes: tapes}, nil
}

// usableCapacity is native capacity less the LTFS reserve (rounded up).
func usableCapacity(native int64) int64 {
	reserve := int64(math.Ceil(float64(native) * ltfsOverheadFraction))

	return native - reserve
}

// par2ReservePercent is the PAR2 percentage Pack reserves: the fixed target or
// the fill-to-capacity floor, clamped to [1, 100].
func par2ReservePercent(redundancy config.Redundancy) int {
	switch {
	case redundancy.TargetPercentage != nil:
		return clampPercent(int(math.Round(*redundancy.TargetPercentage)))
	case redundancy.FillToCapacity != nil:
		// Round up so the reserve is never below the guaranteed floor.
		return clampPercent(int(math.Ceil(redundancy.FillToCapacity.Floor)))
	default:
		return minPAR2Percent
	}
}

// reservedBytes uses par2.MaxOutputBytes, not data×percent/100: par2 pads slices
// to block boundaries and replicates critical packets, so the naive figure runs
// short and let fill-to-capacity tapes overflow Verify (#148).
func reservedBytes(dataBytes int64, sliceCount, percent int) int64 {
	return par2.MaxOutputBytes(dataBytes, sliceCount, percent)
}

func clampPercent(percent int) int {
	if percent < minPAR2Percent {
		return minPAR2Percent
	}

	if percent > maxPAR2Percent {
		return maxPAR2Percent
	}

	return percent
}

func ceilDiv(numerator, denominator int64) int64 {
	return (numerator + denominator - 1) / denominator
}

// packPhase runs the control-side Pack activity and stores the plan. A failure
// aborts the run before any tape is touched.
func packPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	controlCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: packTimeout,
		// Every Pack failure is deterministic, so retrying only delays failing.
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	})

	var activities *PackActivities

	input := PackInput{Config: cfg, Archives: state.staged}

	var plan TapePlan
	if err := workflow.ExecuteActivity(controlCtx, activities.Pack, input).Get(controlCtx, &plan); err != nil {
		return err
	}

	state.plan = plan

	return nil
}
