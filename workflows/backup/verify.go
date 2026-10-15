package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/checksum"
)

// The Verify phase (SPEC §4.3 phase 5) is the hard gate before any tape is
// touched. It independently re-reads and re-hashes every staged slice and PAR2
// file rather than trusting earlier hashes, and confirms each planned tape's
// actual footprint (fill mode may grow PAR2 past the Pack reserve) fits its
// usable capacity.

const (
	// verifyTimeout is generous: Verify re-hashes potentially terabytes.
	verifyTimeout = 24 * time.Hour
)

// VerifyActivities hosts the data-side Verify activity.
type VerifyActivities struct{}

func newVerifyActivities() *VerifyActivities { return &VerifyActivities{} }

// VerifyInput is the payload for the Verify activity.
type VerifyInput struct {
	Plan     TapePlan
	Archives []StagedArchive
	PAR2     []PAR2Set
}

// Verify re-checksums every staged file and confirms each planned tape's tree is
// present and within capacity (SPEC §4.3 phase 5).
func (a *VerifyActivities) Verify(ctx context.Context, input VerifyInput) (VerifiedPlan, error) {
	// Heartbeat so a data-worker restart is caught within
	// activityHeartbeatTimeout, not the 24h StartToClose.
	var verified VerifiedPlan

	err := withActivityHeartbeat(ctx, func() error {
		var err error

		verified, err = verify(ctx, input.Plan, input.Archives, input.PAR2)

		return err
	})
	if err != nil {
		return VerifiedPlan{}, classifyVerifyError(err)
	}

	return verified, nil
}

// classifyVerifyError keeps context cancellation (e.g. graceful worker shutdown)
// retryable, and makes every other failure non-retryable since re-hashing gives
// the same result each attempt, failing the run fast before Load.
func classifyVerifyError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	return temporal.NewNonRetryableApplicationError(err.Error(), "verify-failed", err)
}

func verify(ctx context.Context, plan TapePlan, staged []StagedArchive, sets []PAR2Set) (VerifiedPlan, error) {
	slicesByIndex := make(map[int]StagedArchive, len(staged))
	for _, archive := range staged {
		slicesByIndex[archive.SourceIndex] = archive
	}

	par2ByIndex := make(map[int]PAR2Set, len(sets))
	for _, set := range sets {
		par2ByIndex[set.SourceIndex] = set
	}

	slog.InfoContext(ctx, "verify: re-reading and re-checksumming the staged tree before any tape is loaded",
		"tapes", len(plan.Tapes))

	var totalBytes int64

	for tapeIndex, tape := range plan.Tapes {
		onDiskBytes, err := verifyTape(ctx, tape, slicesByIndex, par2ByIndex)
		if err != nil {
			return VerifiedPlan{}, fmt.Errorf("tape %d: %w", tapeIndex, err)
		}

		totalBytes += onDiskBytes

		slog.InfoContext(ctx, "verify: tape tree verified and within capacity",
			"tapeIndex", tapeIndex, "archives", len(tape.Archives),
			"onDiskBytes", onDiskBytes, "usableCapacityBytes", tape.UsableBytes)
	}

	slog.InfoContext(ctx, "verify: all staged files verified; the run is cleared to load and write tapes",
		"tapes", len(plan.Tapes), "totalBytes", totalBytes)

	return VerifiedPlan{}, nil
}

// verifyTape verifies one planned tape's tree and returns its on-disk bytes. A
// planned archive never staged or lacking a PAR2 set is a failure.
func verifyTape(ctx context.Context, tape PlannedTape, slicesByIndex map[int]StagedArchive, par2ByIndex map[int]PAR2Set) (int64, error) {
	var onDiskBytes int64

	for _, placement := range tape.Archives {
		archive, ok := slicesByIndex[placement.SourceIndex]
		if !ok {
			return 0, fmt.Errorf("sources[%d] is planned onto this tape but was not staged", placement.SourceIndex)
		}

		set, ok := par2ByIndex[placement.SourceIndex]
		if !ok {
			return 0, fmt.Errorf("sources[%d] is planned onto this tape but has no PAR2 recovery set", placement.SourceIndex)
		}

		sliceBytes, err := verifyFiles(ctx, archive.Slices)
		if err != nil {
			return 0, fmt.Errorf("sources[%d] slices: %w", placement.SourceIndex, err)
		}

		par2Bytes, err := verifyFiles(ctx, set.Files)
		if err != nil {
			return 0, fmt.Errorf("sources[%d] PAR2 set: %w", placement.SourceIndex, err)
		}

		onDiskBytes += sliceBytes + par2Bytes
	}

	if onDiskBytes > tape.UsableBytes {
		return 0, fmt.Errorf("planned tree is %d bytes, exceeding the tape's usable capacity of %d bytes", onDiskBytes, tape.UsableBytes)
	}

	return onDiskBytes, nil
}

// verifyFiles re-checksums each staged file and totals their recorded sizes,
// which a passing checksum proves are the true sizes.
func verifyFiles(ctx context.Context, files []StagedSlice) (int64, error) {
	var total int64

	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		if err := checksum.Verify(file.Path, file.SHA256); err != nil {
			return 0, err
		}

		total += file.SizeBytes
	}

	return total, nil
}

// verifyPhase runs Verify and stores the VerifiedPlan; a failure aborts the run
// before any tape is loaded.
func verifyPhase(ctx workflow.Context, _ config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: verifyTimeout,
		// Deterministic failures come back non-retryable, so retrying here only
		// covers worker restarts.
		HeartbeatTimeout: activityHeartbeatTimeout,
	})

	var activities *VerifyActivities

	input := VerifyInput{Plan: state.plan, Archives: state.staged, PAR2: state.par2}

	var verified VerifiedPlan
	if err := workflow.ExecuteActivity(dataCtx, activities.Verify, input).Get(dataCtx, &verified); err != nil {
		return err
	}

	state.verified = verified

	return nil
}
