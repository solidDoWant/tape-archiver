package backup

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
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

	sets, err := generatePAR2(t.Context(), cfg, plan, staged)
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
func TestVerifyFailureBlocksLoad(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

	failPhase(t, env, PhaseVerify)

	env.ExecuteWorkflow(Backup, config.Config{})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())

	// The Load phase's activity is never invoked once Verify fails.
	env.AssertNotCalled(t, "loadActivity")
}
