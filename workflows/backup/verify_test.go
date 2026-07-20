package backup

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
)

// verifyFixture builds a real staged tree on disk — staged archives, a Pack plan,
// and generated PAR2 sets — so the Verify tests run against the same artifacts the
// pipeline produces. The slices and recovery files are real files with correct
// recorded checksums and sizes.
func verifyFixture(t *testing.T) (TapePlan, []StagedArchive, []PAR2Set) {
	t.Helper()

	staged := []StagedArchive{
		writeStagedArchive(t, 0, []int{100_000, 100_000}),
		writeStagedArchive(t, 1, []int{120_000}),
	}

	// A generous tape capacity so both archives pack onto one tape and the real
	// staged tree fits: par2cmdline's per-block overhead inflates the recovery set
	// far past its nominal percentage at these tiny test-block sizes (negligible at
	// LTO's TB scale), which would otherwise overflow a small capacity.
	cfg := packConfig(500_000_000, 1, 1, targetRedundancy(20))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)

	sets, err := generatePAR2(t.Context(), cfg, plan, staged, nil)
	require.NoError(t, err)

	return plan, staged, sets
}

// corruptFile flips a byte in the file at path, so its contents no longer match
// the SHA-256 recorded when it was staged.
func corruptFile(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	data[0] ^= 0xFF

	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// TestVerifyAcceptsIntactStagedTree covers AC1: when every staged file's checksum
// matches and each tape's tree is present and within capacity, Verify succeeds so
// the workflow can proceed to Load.
func TestVerifyAcceptsIntactStagedTree(t *testing.T) {
	t.Parallel()

	plan, staged, sets := verifyFixture(t)

	_, err := verify(t.Context(), plan, staged, sets)
	require.NoError(t, err)
}

// TestVerifyFailsOnChecksumMismatch covers AC2: a staged file whose contents have
// changed since it was staged — whether an archive slice or a PAR2 recovery file —
// fails verification, so the run cannot proceed to Load.
func TestVerifyFailsOnChecksumMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// corrupt picks the file to corrupt from the staged tree.
		corrupt func(staged []StagedArchive, sets []PAR2Set) string
	}{
		{
			name: "archive slice",
			corrupt: func(staged []StagedArchive, _ []PAR2Set) string {
				return staged[0].Slices[0].Path
			},
		},
		{
			name: "PAR2 recovery file",
			corrupt: func(_ []StagedArchive, sets []PAR2Set) string {
				return sets[0].Files[0].Path
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plan, staged, sets := verifyFixture(t)

			corruptFile(t, test.corrupt(staged, sets))

			_, err := verify(t.Context(), plan, staged, sets)
			require.Error(t, err)
		})
	}
}

// TestVerifyFailsOnMissingFile covers AC3: a missing file from a planned tape's
// tree fails the run before Load.
func TestVerifyFailsOnMissingFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// remove picks the file to delete from the staged tree.
		remove func(staged []StagedArchive, sets []PAR2Set) string
	}{
		{
			name: "archive slice",
			remove: func(staged []StagedArchive, _ []PAR2Set) string {
				return staged[1].Slices[0].Path
			},
		},
		{
			name: "PAR2 recovery file",
			remove: func(_ []StagedArchive, sets []PAR2Set) string {
				return sets[1].Files[0].Path
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plan, staged, sets := verifyFixture(t)

			require.NoError(t, os.Remove(test.remove(staged, sets)))

			_, err := verify(t.Context(), plan, staged, sets)
			require.Error(t, err)
		})
	}
}

// TestVerifyFailsWhenArchiveAbsentFromStagedTree covers AC3: a tape that plans an
// archive which was never staged, or never given a PAR2 set, has an incomplete
// tree and fails before Load.
func TestVerifyFailsWhenArchiveAbsentFromStagedTree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// drop returns the staged archives and PAR2 sets to pass, with one of the
		// planned archives' artifacts removed from the input entirely.
		drop func(staged []StagedArchive, sets []PAR2Set) ([]StagedArchive, []PAR2Set)
	}{
		{
			name: "not staged",
			drop: func(staged []StagedArchive, sets []PAR2Set) ([]StagedArchive, []PAR2Set) {
				return staged[:1], sets
			},
		},
		{
			name: "no PAR2 set",
			drop: func(staged []StagedArchive, sets []PAR2Set) ([]StagedArchive, []PAR2Set) {
				return staged, sets[:1]
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plan, staged, sets := verifyFixture(t)

			gotStaged, gotSets := test.drop(staged, sets)

			_, err := verify(t.Context(), plan, gotStaged, gotSets)
			require.Error(t, err)
		})
	}
}

// TestVerifyFailsWhenPlannedTreeExceedsCapacity covers AC3: even when every file
// checksums correctly, a tape whose complete tree does not fit within its usable
// capacity fails the run before Load.
func TestVerifyFailsWhenPlannedTreeExceedsCapacity(t *testing.T) {
	t.Parallel()

	_, staged, sets := verifyFixture(t)

	// A plan whose tape capacity is far below the staged tree's real size: every
	// file still checksums correctly, but the tree cannot fit, so Verify rejects
	// the plan.
	tightPlan := TapePlan{
		Copies: 1,
		Tapes: []PlannedTape{{
			Archives: []PlannedArchive{
				{SourceIndex: 0, DataBytes: staged[0].SizeBytes},
				{SourceIndex: 1, DataBytes: staged[1].SizeBytes},
			},
			UsableBytes: 1_000,
		}},
	}

	_, err := verify(t.Context(), tightPlan, staged, sets)
	require.Error(t, err)
}

// TestVerifyFailureBlocksLoad covers AC2/AC3's "no tape is loaded" guarantee at
// the workflow level: when the Verify phase fails, the run aborts before the Load
// phase ever runs.
//
// The test is constructed so that a regression which swallowed the Verify failure
// *would* reach the Load phase: Pack is mocked to return a non-empty plan and the
// run is given a real library (one drive, one blank slot, a changer), so the tape
// path yields exactly one drive-set and Load becomes reachable. Load is then routed
// through OnActivity to flip a captured flag if it is ever dispatched — so the test
// observes directly whether Load ran, rather than asserting on an unmocked name.
func TestVerifyFailureBlocksLoad(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

	// A valid config is required by the entry-point validation gate, but resolve it
	// to nothing so the run stages no sources without touching a real pool.
	expectResolveEmpty(env)

	// A non-empty plan makes planDriveSets yield a drive-set, so Load is reachable
	// once the run passes Verify. Generate PAR2 stays a no-op: the run stages no
	// sources, so the staged tree is empty and generatePAR2 returns early.
	plan, _, _ := seededPlan(1, 1)
	env.OnActivity((&PackActivities{}).Pack, mock.Anything, mock.Anything).
		Return(plan, nil)

	// Fail the Verify phase.
	failPhase(t, env, PhaseVerify)

	// Observe Load directly: it flips loadCalled and errors if it is ever invoked.
	loadCalled := false

	env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ LoadInput) ([]LoadedTape, error) {
			loadCalled = true

			return nil, errors.New("Load invoked after Verify failed")
		})

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())

	// The Load phase's activity is never invoked once Verify fails.
	assert.False(t, loadCalled, "Load ran after Verify failed")
}

// TestVerifyRetriesTransientFailure covers AC1: a routine data-worker restart
// mid-Verify — a retryable, non-application error — is retried and the run
// proceeds to completion, rather than being capped at one attempt as the old
// RetryPolicy{MaximumAttempts: 1} would have. The Verify activity fails once then
// succeeds; with the heartbeat-based default policy the workflow must complete.
func TestVerifyRetriesTransientFailure(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	expectResolveEmpty(env)
	expectReportDeliverSuccess(env)

	env.OnActivity((&VerifyActivities{}).Verify, mock.Anything, mock.Anything).
		Return(VerifiedPlan{}, errors.New("data worker restarted")).Once()
	env.OnActivity((&VerifyActivities{}).Verify, mock.Anything, mock.Anything).
		Return(VerifiedPlan{}, nil)

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

// TestVerifyCancellationStaysRetryable covers AC1's graceful-shutdown case: when
// the activity context is cancelled over an otherwise intact staged tree — as a
// data-worker shutdown does — the Verify activity returns a retryable error
// wrapping context.Canceled, never a non-retryable ApplicationError, so the
// rescheduled attempt re-runs the idempotent re-read.
func TestVerifyCancellationStaysRetryable(t *testing.T) {
	t.Parallel()

	plan, staged, sets := verifyFixture(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := newVerifyActivities().Verify(ctx, VerifyInput{Plan: plan, Archives: staged, PAR2: sets})
	require.ErrorIs(t, err, context.Canceled)

	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		assert.False(t, appErr.NonRetryable(), "a cancelled context must stay retryable")
	}
}

// TestVerifyClassifiesDeterministicFaultsNonRetryable covers AC2: a deterministic
// verification fault — a corrupted slice, a missing file, or an over-capacity plan
// — is surfaced by the Verify activity as a non-retryable ApplicationError, so the
// default retry policy fails the run fast (before Load) instead of retrying a fault
// that recurs on every attempt.
func TestVerifyClassifiesDeterministicFaultsNonRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// mutate breaks the fixture so verification fails deterministically.
		mutate func(t *testing.T, plan *TapePlan, staged []StagedArchive, sets []PAR2Set)
	}{
		{
			name: "checksum mismatch",
			mutate: func(t *testing.T, _ *TapePlan, staged []StagedArchive, _ []PAR2Set) {
				corruptFile(t, staged[0].Slices[0].Path)
			},
		},
		{
			name: "missing file",
			mutate: func(t *testing.T, _ *TapePlan, staged []StagedArchive, _ []PAR2Set) {
				require.NoError(t, os.Remove(staged[1].Slices[0].Path))
			},
		},
		{
			name: "over-capacity plan",
			mutate: func(_ *testing.T, plan *TapePlan, _ []StagedArchive, _ []PAR2Set) {
				plan.Tapes[0].UsableBytes = 1_000
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plan, staged, sets := verifyFixture(t)

			test.mutate(t, &plan, staged, sets)

			_, err := newVerifyActivities().Verify(t.Context(), VerifyInput{Plan: plan, Archives: staged, PAR2: sets})
			require.Error(t, err)

			var appErr *temporal.ApplicationError
			require.ErrorAs(t, err, &appErr)
			assert.True(t, appErr.NonRetryable(), "a deterministic verification fault must be non-retryable")
		})
	}
}
