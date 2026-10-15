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

// verifyFixture builds a real staged tree (archives, Pack plan, PAR2 sets) on
// disk with correct recorded checksums and sizes.
func verifyFixture(t *testing.T) (TapePlan, []StagedArchive, []PAR2Set) {
	t.Helper()

	staged := []StagedArchive{
		writeStagedArchive(t, 0, []int{100_000, 100_000}),
		writeStagedArchive(t, 1, []int{120_000}),
	}

	// Generous capacity: par2's per-block overhead inflates the recovery set far
	// past its nominal percentage at these tiny block sizes.
	cfg := packConfig(500_000_000, 1, 1, targetRedundancy(20))

	plan, err := pack(cfg, staged)
	require.NoError(t, err)

	sets, err := generatePAR2(t.Context(), cfg, plan, staged, nil)
	require.NoError(t, err)

	return plan, staged, sets
}

// corruptFile flips a byte so the file no longer matches its recorded SHA-256.
func corruptFile(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	data[0] ^= 0xFF

	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// TestVerifyAcceptsIntactStagedTree covers AC1: an intact tree within capacity
// passes Verify.
func TestVerifyAcceptsIntactStagedTree(t *testing.T) {
	t.Parallel()

	plan, staged, sets := verifyFixture(t)

	_, err := verify(t.Context(), plan, staged, sets)
	require.NoError(t, err)
}

// TestVerifyFailsOnChecksumMismatch covers AC2: a changed slice or PAR2 file
// fails verification.
func TestVerifyFailsOnChecksumMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
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

// TestVerifyFailsOnMissingFile covers AC3: a missing file fails before Load.
func TestVerifyFailsOnMissingFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
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

// TestVerifyFailsWhenArchiveAbsentFromStagedTree covers AC3: a planned archive
// never staged or never given a PAR2 set fails before Load.
func TestVerifyFailsWhenArchiveAbsentFromStagedTree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// drop removes one planned archive's artifacts from the input.
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

// TestVerifyFailsWhenPlannedTreeExceedsCapacity covers AC3: correct checksums
// but an over-capacity tree still fails before Load.
func TestVerifyFailsWhenPlannedTreeExceedsCapacity(t *testing.T) {
	t.Parallel()

	_, staged, sets := verifyFixture(t)

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

// TestVerifyFailureBlocksLoad covers AC2/AC3's "no tape is loaded" guarantee.
// Pack returns a non-empty plan and the library is real, so Load is reachable if
// a regression swallowed the Verify failure; Load flips a flag if dispatched.
func TestVerifyFailureBlocksLoad(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

	// Valid config, resolved to nothing so no real pool is touched.
	expectResolveEmpty(env)

	// A non-empty plan makes Load reachable; PAR2 is a no-op on the empty tree.
	plan, _, _ := seededPlan(1, 1)
	env.OnActivity((&PackActivities{}).Pack, mock.Anything, mock.Anything).
		Return(plan, nil)

	failPhase(t, env, PhaseVerify)

	loadCalled := false

	env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ LoadInput) ([]LoadedTape, error) {
			loadCalled = true

			return nil, errors.New("Load invoked after Verify failed")
		})

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())

	assert.False(t, loadCalled, "Load ran after Verify failed")
}

// TestVerifyRetriesTransientFailure covers AC1: a retryable failure (worker
// restart) is retried and the run completes, rather than being capped at one
// attempt as the old MaximumAttempts: 1 policy did.
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

// TestVerifyCancellationStaysRetryable covers AC1's graceful-shutdown case: a
// cancelled context yields a retryable error wrapping context.Canceled.
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

// TestVerifyClassifiesDeterministicFaultsNonRetryable covers AC2: corruption,
// a missing file, or over-capacity yields a non-retryable ApplicationError.
func TestVerifyClassifiesDeterministicFaultsNonRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
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
