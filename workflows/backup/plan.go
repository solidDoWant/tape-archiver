package backup

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// The Pack phase (SPEC §4.3 phase 3) bin-packs the prepared archives onto tapes
// by their *measured* staged size — never the Resolve estimate — replicated
// across the configured number of copies. Each tape's planned contents (archive
// data plus the PAR2 recovery space reserved for it) must fit within the tape's
// native capacity after subtracting LTFS filesystem overhead, so a later phase
// never overruns a tape. The PAR2 space reserved here is the minimum (the fixed
// target percentage, or the fill-to-capacity floor); the Generate PAR2 phase
// grows recovery sets into each tape's remaining slack in fill mode (SPEC §4.3
// phases 3–4).
//
// Packing is pure planning over the measured sizes — no disk or device access —
// so it runs on the control worker (SPEC §4.1) and is unit-testable directly.

const (
	// packTimeout bounds the Pack activity. Bin-packing is in-memory arithmetic
	// over the staged work list, so a few minutes is far more than enough.
	packTimeout = 5 * time.Minute

	// ltfsOverheadFraction is the fraction of a tape's native capacity reserved
	// for LTFS filesystem overhead — the index partition, the on-tape index and
	// directory metadata, and the per-tape manifest/checksum file (SPEC §6) — so
	// a tape packed to its usable capacity still physically fits once LTFS is laid
	// down. It is a deliberately conservative estimate (the archive payload is a
	// handful of large age slices, so the index itself is tiny relative to this);
	// it is an internal planning constant, not a run-config field, and can be
	// tuned without changing any user-facing surface. SPEC §4.3 requires planning
	// against native capacity with hardware compression disabled.
	ltfsOverheadFraction = 0.005

	// minPAR2Percent and maxPAR2Percent bound the integer redundancy percentage
	// par2 accepts (SPEC §8); par2cmdline rejects anything outside [1, 100].
	minPAR2Percent = 1
	maxPAR2Percent = 100
)

// PackActivities hosts the control-side Pack activity. It carries no
// dependencies: packing is pure arithmetic over the measured staged sizes.
type PackActivities struct{}

// newPackActivities returns the control-side Pack activity.
func newPackActivities() *PackActivities { return &PackActivities{} }

// PackInput is the payload for the Pack activity: the run config (for capacity,
// copy count, and the PAR2 reserve) and the staged work list to bin-pack.
type PackInput struct {
	Config   config.Config
	Archives []StagedArchive
}

// Pack bin-packs the staged archives onto tapes (SPEC §4.3 phase 3), returning
// the TapePlan the Generate PAR2 and write phases build on.
func (a *PackActivities) Pack(_ context.Context, input PackInput) (TapePlan, error) {
	return pack(input.Config, input.Archives)
}

// pack is the pure bin-packing the Pack activity wraps. It is split out so it can
// be exercised without an activity context.
func pack(cfg config.Config, staged []StagedArchive) (TapePlan, error) {
	// Nothing to place: return an empty plan that still records the copy count,
	// so downstream phases see a well-formed plan for a no-source run.
	if len(staged) == 0 {
		return TapePlan{Copies: cfg.Copies}, nil
	}

	copies := cfg.Copies

	if copies < 1 {
		return TapePlan{}, fmt.Errorf("copies must be at least 1, got %d", copies)
	}

	// Copies may exceed the drive count: the tape path writes the copies of each
	// logical tape in successive drive-sets of at most len(Drives) at a time
	// (SPEC §4.3 phases 6–8), so the plan is not bounded by the drive count.

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
			PAR2ReservedBytes: reservedBytes(archive.SizeBytes, reservePercent),
		}

		// The measured size can exceed the Resolve feasibility estimate, so an
		// archive that passed the pre-check may still be too large for one tape.
		// Reject it here, before any tape is touched, naming the source.
		if footprint := item.Footprint(); footprint > usable {
			return TapePlan{}, fmt.Errorf(
				"sources[%d] measured footprint %d bytes (data %d + PAR2 %d) exceeds one tape's usable capacity %d bytes",
				archive.SourceIndex, footprint, item.DataBytes, item.PAR2ReservedBytes, usable,
			)
		}

		items[index] = item
	}

	// First-fit-decreasing: place the largest footprints first so smaller
	// archives fill the gaps they leave. This keeps the tape count low without
	// solving the (NP-hard) optimal bin-packing.
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

	// Present each tape's archives in source order for a stable, readable plan;
	// the bin-packing order above is by footprint, not source.
	for index := range tapes {
		sort.Slice(tapes[index].Archives, func(i, j int) bool {
			return tapes[index].Archives[i].SourceIndex < tapes[index].Archives[j].SourceIndex
		})
	}

	return TapePlan{Copies: copies, Tapes: tapes}, nil
}

// usableCapacity is the per-tape capacity available for archive data and PAR2:
// the native capacity less the LTFS filesystem overhead reserve, rounded so the
// reserve is never understated.
func usableCapacity(native int64) int64 {
	reserve := int64(math.Ceil(float64(native) * ltfsOverheadFraction))

	return native - reserve
}

// par2ReservePercent is the integer PAR2 redundancy the Pack phase reserves space
// for per archive: the fixed target, or the fill-to-capacity floor — the minimum
// footprint, since fill only grows parity into otherwise-wasted tape space and so
// cannot make an archive that fits at the floor stop fitting (mirrors the Resolve
// pre-check's par2Fraction rationale). It is rounded to the whole percent par2
// accepts and clamped to [1, 100].
func par2ReservePercent(redundancy config.Redundancy) int {
	switch {
	case redundancy.TargetPercentage != nil:
		return clampPercent(int(math.Round(*redundancy.TargetPercentage)))
	case redundancy.FillToCapacity != nil:
		// Round the floor up so the reserved minimum is never below the floor
		// redundancy the Generate PAR2 phase guarantees.
		return clampPercent(int(math.Ceil(redundancy.FillToCapacity.Floor)))
	default:
		return minPAR2Percent
	}
}

// reservedBytes is the PAR2 footprint to reserve for an archive of dataBytes at
// the given integer percentage, rounded up so the reservation never runs short.
func reservedBytes(dataBytes int64, percent int) int64 {
	return ceilDiv(dataBytes*int64(percent), 100)
}

// clampPercent clamps an integer percentage to the [1, 100] range par2 accepts.
func clampPercent(percent int) int {
	if percent < minPAR2Percent {
		return minPAR2Percent
	}

	if percent > maxPAR2Percent {
		return maxPAR2Percent
	}

	return percent
}

// ceilDiv returns ceil(numerator / denominator) for non-negative numerator and
// positive denominator.
func ceilDiv(numerator, denominator int64) int64 {
	return (numerator + denominator - 1) / denominator
}

// packPhase orchestrates the Pack phase (SPEC §4.3 phase 3): it runs the
// control-side Pack activity over the staged work list and stores the resulting
// plan in runState for the Generate PAR2 phase. A failure aborts the run here,
// before any tape is touched.
func packPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	controlCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: packTimeout,
		// Pack is a pure in-memory bin-pack that ignores its context: every failure
		// (copies < 1, capacity too small after overhead, an archive too large for
		// one tape) is deterministic and recurs identically on retry. Cap attempts
		// at 1 so a permanent plan fault fails the run at once instead of retrying
		// under the default policy until the timeout — the same rationale as Verify.
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
