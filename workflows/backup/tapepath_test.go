package backup

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// tapePathTestParams seeds a runState for the tapePathTestWorkflow: the plan to
// write and the staged/PAR2 trees the Write phase assembles per tape.
type tapePathTestParams struct {
	Cfg    config.Config
	Plan   TapePlan
	Staged []StagedArchive
	PAR2   []PAR2Set
}

// tapePathTestWorkflow drives only the tape path (runTapePath) over a seeded plan
// so the drive-set orchestration can be tested with mocked Load/Write/Eject
// activities, without the full backup pipeline building a plan first.
func tapePathTestWorkflow(ctx workflow.Context, params tapePathTestParams) ([]WrittenTape, error) {
	state := &runState{plan: params.Plan, staged: params.Staged, par2: params.PAR2}

	failing := ""
	if err := runTapePath(ctx, params.Cfg, state, &failing); err != nil {
		return nil, fmt.Errorf("%s: %w", failing, err)
	}

	return state.written, nil
}

// newTapePathEnv registers the tape-path test workflow and every tape-path
// activity so a test attaches behavior with OnActivity. The Write phase creates a
// Temporal session, so the session worker is enabled.
func newTapePathEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(tapePathTestWorkflow)

	env.SetWorkerOptions(worker.Options{EnableSessionWorker: true})

	env.RegisterActivity(newLoadActivities())

	registry := newMountRegistry()
	env.RegisterActivity(newWriteActivities(registry, t.TempDir()))
	env.RegisterActivity(newTeardownActivities(registry))
	env.RegisterActivity(&WriteHealthActivities{})
	env.RegisterActivity(newEjectActivities())

	return env
}

// tapePathConfig returns a config with the given library drives and enough blank
// slots to cover copyCount copies of tapeCount logical tapes.
func tapePathConfig(drives, tapeCount, copyCount int) config.Config {
	driveNodes := make([]string, drives)
	for i := range driveNodes {
		driveNodes[i] = fmt.Sprintf("/dev/nst%d", i)
	}

	blankSlots := make([]int, tapeCount*copyCount)
	for i := range blankSlots {
		blankSlots[i] = 100 + i
	}

	return config.Config{
		Copies: copyCount,
		Library: config.Library{
			Changer:           "/dev/sch0",
			Drives:            driveNodes,
			BlankSlots:        blankSlots,
			TapeCapacityBytes: 2_500_000_000_000,
		},
	}
}

// seededPlan builds a plan of tapeCount logical tapes, each carrying one archive
// (source index = tape index), plus the matching staged and PAR2 trees so
// archivesForTape resolves every placement.
func seededPlan(tapeCount, copyCount int) (TapePlan, []StagedArchive, []PAR2Set) {
	tapes := make([]PlannedTape, tapeCount)
	staged := make([]StagedArchive, tapeCount)
	par2 := make([]PAR2Set, tapeCount)

	for i := 0; i < tapeCount; i++ {
		tapes[i] = PlannedTape{Archives: []PlannedArchive{{SourceIndex: i, DataBytes: 1000}}}
		staged[i] = StagedArchive{
			SourceIndex: i,
			SizeBytes:   1000,
			Slices:      []StagedSlice{{Path: fmt.Sprintf("/staging/%d/slice.000", i), SHA256: "abc", SizeBytes: 1000}},
		}
		par2[i] = PAR2Set{
			SourceIndex: i,
			Files:       []StagedSlice{{Path: fmt.Sprintf("/staging/%d/slice.par2", i), SHA256: "def", SizeBytes: 10}},
		}
	}

	return TapePlan{Copies: copyCount, Tapes: tapes}, staged, par2
}

// mockLoadReturnsAssignments mocks the Load activity to return one LoadedTape per
// assignment in the drive-set, echoing the (tape, copy) assignment back with a
// synthetic barcode/device. It appends each loaded drive-set's size to setSizes
// (under mu) so a test can count sets and check the concurrency bound.
func mockLoadReturnsAssignments(env *testsuite.TestWorkflowEnvironment, mu *sync.Mutex, setSizes *[]int) {
	env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input LoadInput) ([]LoadedTape, error) {
			mu.Lock()

			*setSizes = append(*setSizes, len(input.Tapes))
			mu.Unlock()

			loaded := make([]LoadedTape, len(input.Tapes))
			for i, assignment := range input.Tapes {
				loaded[i] = LoadedTape{
					Barcode:    tape.Barcode(fmt.Sprintf("BC-%d-%d", assignment.TapeIndex, assignment.CopyIndex)),
					DriveIndex: i,
					TapeIndex:  assignment.TapeIndex,
					CopyIndex:  assignment.CopyIndex,
					SourceSlot: assignment.BlankSlot,
					STDevice:   assignment.Drive,
					SGDevice:   fmt.Sprintf("/dev/sg%d", i),
				}
			}

			return loaded, nil
		})
}

// TestRunTapePathMultipleDriveSets drives a plan that needs several drive-sets
// from both extra logical tapes and extra copies, and asserts every (tape, copy)
// pair is written and ejected with concurrency bounded by the drive count
// (issue #66 AC1, AC2, AC3, AC4).
func TestRunTapePathMultipleDriveSets(t *testing.T) {
	t.Parallel()

	const (
		drives = 2
		tapes  = 3
		copies = 3
	)

	env := newTapePathEnv(t)

	var mu sync.Mutex

	// loadSetSizes records the size of each drive-set as it is loaded.
	var loadSetSizes []int

	mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

	writeArchives := make(map[[2]int][]TapeWriteArchive)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input WriteTreeInput) error {
			mu.Lock()
			writeArchives[[2]int{input.TapeIndex, input.CopyIndex}] = input.Archives
			mu.Unlock()

			return nil
		})
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		[]byte("<ltfsindex></ltfsindex>"), nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		WriteHealth{}, nil)
	env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(nil)

	var ejectedBarcodes []tape.Barcode

	ejectCalls := 0

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input EjectInput) error {
			mu.Lock()
			ejectCalls++

			for _, wt := range input.WrittenTapes {
				ejectedBarcodes = append(ejectedBarcodes, wt.Barcode)
			}
			mu.Unlock()

			return nil
		})

	plan, staged, par2 := seededPlan(tapes, copies)

	env.ExecuteWorkflow(tapePathTestWorkflow, tapePathTestParams{
		Cfg:    tapePathConfig(drives, tapes, copies),
		Plan:   plan,
		Staged: staged,
		PAR2:   par2,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var written []WrittenTape
	require.NoError(t, env.GetWorkflowResult(&written))

	// AC1/AC2: every (tape, copy) pair is written exactly once.
	require.Len(t, written, tapes*copies, "every physical tape must be written")

	writtenPairs := make(map[[2]int]bool)
	for _, wt := range written {
		writtenPairs[[2]int{wt.TapeIndex, wt.CopyIndex}] = true
	}

	for tapeIndex := 0; tapeIndex < tapes; tapeIndex++ {
		for copyIndex := 0; copyIndex < copies; copyIndex++ {
			assert.True(t, writtenPairs[[2]int{tapeIndex, copyIndex}],
				"tape %d copy %d must be written", tapeIndex, copyIndex)
		}
	}

	// AC3: no drive-set ever exceeds the drive count.
	expectedSets := (tapes*copies + drives - 1) / drives
	assert.Len(t, loadSetSizes, expectedSets, "one Load per drive-set")
	assert.Equal(t, expectedSets, ejectCalls, "one Eject per drive-set")

	for _, size := range loadSetSizes {
		assert.LessOrEqual(t, size, drives, "a drive-set must not exceed the drive count")
	}

	// AC4: the copies of a logical tape are written from the same staged tree.
	for tapeIndex := 0; tapeIndex < tapes; tapeIndex++ {
		first := writeArchives[[2]int{tapeIndex, 0}]
		require.NotNil(t, first, "tape %d copy 0 must have been written", tapeIndex)

		for copyIndex := 1; copyIndex < copies; copyIndex++ {
			assert.Equal(t, first, writeArchives[[2]int{tapeIndex, copyIndex}],
				"tape %d copy %d must write the same staged tree as copy 0", tapeIndex, copyIndex)
		}
	}

	// Every written tape is ejected.
	assert.Len(t, ejectedBarcodes, tapes*copies, "every written tape must be ejected")
}

// TestRunTapePathStopsAfterSetFailure verifies that when a tape in a drive-set
// fails its write, the run fails for that set and no later set is loaded
// (issue #66 AC5).
func TestRunTapePathStopsAfterSetFailure(t *testing.T) {
	t.Parallel()

	const (
		drives = 1 // one physical tape per set, so failure lands in the first set
		tapes  = 3
		copies = 1
	)

	env := newTapePathEnv(t)

	var mu sync.Mutex

	var loadSetSizes []int

	mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

	// The first (and only) tape in set 0 fails to format; the Write phase fails,
	// so runTapePath returns before any later set loads.
	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		fmt.Errorf("simulated format failure"))
	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		[]byte("<ltfsindex></ltfsindex>"), nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		WriteHealth{}, nil)
	env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(nil)

	ejectCalls := 0

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ EjectInput) error {
			mu.Lock()
			ejectCalls++
			mu.Unlock()

			return nil
		})

	plan, staged, par2 := seededPlan(tapes, copies)

	env.ExecuteWorkflow(tapePathTestWorkflow, tapePathTestParams{
		Cfg:    tapePathConfig(drives, tapes, copies),
		Plan:   plan,
		Staged: staged,
		PAR2:   par2,
	})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "a set's write failure must fail the run")
	assert.Contains(t, err.Error(), PhaseWrite, "the failure must be attributed to the Write phase")

	mu.Lock()
	defer mu.Unlock()

	assert.Len(t, loadSetSizes, 1, "only the failing set may be loaded; no later set is loaded")
	assert.Equal(t, 0, ejectCalls, "a failed set is not ejected")
}
