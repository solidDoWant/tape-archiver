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
// touched: "no data is written to tape until the complete contents of every tape
// are staged and verified on disk." It re-reads every staged file — the archive
// slices and the PAR2 recovery files — recomputes its SHA-256 and compares it
// against the digest recorded by Prepare and Generate PAR2. This is an
// independent re-read straight from disk, not a trust of the earlier phases'
// hashes, so it catches any corruption or truncation that happened to the staged
// bytes since they were written.
//
// It then confirms, for every planned tape, that the tape's complete tree is
// present on disk and that its real on-disk footprint — the measured data plus
// the *actual* PAR2 set, which fill-to-capacity mode may have grown past the Pack
// reserve — fits within that tape's usable capacity. Any checksum mismatch,
// missing file, or over-capacity tape fails the run here, before Load.
//
// The staged bytes live on the data worker (SPEC §4.1), so this phase runs on the
// data queue, reading the files by the absolute paths Prepare and Generate PAR2
// recorded.

const (
	// verifyTimeout bounds the whole Verify activity. It re-reads every staged
	// byte — archive slices and PAR2 recovery files, potentially terabytes —
	// through SHA-256, so the bound is as generous as Prepare's and Generate
	// PAR2's.
	verifyTimeout = 24 * time.Hour
)

// VerifyActivities hosts the data-side Verify activity. It carries no
// dependencies: verification re-reads staged files by their recorded absolute
// paths and digests them through pkg/checksum.
type VerifyActivities struct{}

// newVerifyActivities returns the data-side Verify activity.
func newVerifyActivities() *VerifyActivities { return &VerifyActivities{} }

// VerifyInput is the payload for the Verify activity: the Pack plan (the
// authoritative per-tape tree and each tape's usable capacity), the staged work
// list (the archive slices to re-check), and the PAR2 recovery sets (the recovery
// files to re-check).
type VerifyInput struct {
	Plan     TapePlan
	Archives []StagedArchive
	PAR2     []PAR2Set
}

// Verify re-reads and re-checksums every staged file and confirms each planned
// tape's complete tree is present on disk and within capacity (SPEC §4.3 phase
// 5), returning the VerifiedPlan the run needs before any tape is loaded. It
// returns a non-nil error — and no VerifiedPlan — on any checksum mismatch,
// missing file, or over-capacity tape.
func (a *VerifyActivities) Verify(ctx context.Context, input VerifyInput) (VerifiedPlan, error) {
	// Emit a liveness heartbeat while re-reading the staged tree so a data-worker
	// restart mid-Verify is caught within activityHeartbeatTimeout rather than the
	// 24h StartToClose (the same pattern PrepareArchives and GeneratePAR2 use).
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

// classifyVerifyError maps a verify() failure to its Temporal retry semantics so a
// routine worker restart is retried while a genuine verification failure still
// aborts the run before Load. A context cancellation — a graceful data-worker
// shutdown makes verifyFiles return ctx.Err() — is an infrastructure fault, not a
// verification fault: it is returned unwrapped so it stays retryable and the
// rescheduled attempt re-runs the idempotent re-read. Every other verify() failure
// (checksum mismatch, missing file, unstaged or PAR2-less tree, or over-capacity
// plan) is deterministic — the re-read and re-hash produce the same result on every
// attempt — so it is wrapped non-retryable to fail the run fast, before Load, the
// same fast-fail the old MaximumAttempts:1 policy provided.
func classifyVerifyError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	return temporal.NewNonRetryableApplicationError(err.Error(), "verify-failed", err)
}

// verify is the body of the Verify activity, split out so it can be exercised
// without an activity context.
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

// verifyTape verifies one planned tape's complete tree: every assigned archive's
// slices and PAR2 recovery set are present on disk and match their recorded
// SHA-256, and the tape's total on-disk footprint stays within its usable
// capacity. A planned archive that was never staged or never given a PAR2 set is
// itself a missing-tree failure.
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

// verifyFiles re-reads each staged file, confirms it matches its recorded
// SHA-256, and returns the total of their recorded sizes. A file that is missing
// or whose contents have changed since it was staged fails verification; because
// a passing checksum guarantees the bytes are identical to when they were
// measured, the recorded sizes are the true on-disk sizes. The context is checked
// between files so the long re-hash honors cancellation.
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

// verifyPhase orchestrates the Verify phase (SPEC §4.3 phase 5): it runs the
// data-side Verify activity over the Pack plan, the staged work list, and the
// PAR2 recovery sets, and stores the VerifiedPlan in runState for the Load phase.
// A failure aborts the run here — before any tape is loaded — which is the whole
// point of the phase.
func verifyPhase(ctx workflow.Context, _ config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: verifyTimeout,
		// A liveness heartbeat plus a short HeartbeatTimeout (the Prepare/PAR2
		// pattern) lets a routine data-worker restart mid-Verify be caught and
		// rescheduled within activityHeartbeatTimeout rather than stranding the run
		// to the 24h StartToClose. Deterministic verification failures (checksum
		// mismatch, missing file, over-capacity plan) still abort the run before
		// Load: the Verify activity surfaces them as non-retryable ApplicationErrors,
		// so the default retry policy does not retry the permanent fault.
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
