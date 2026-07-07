package backup

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

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

// TestRunTapePathPassesAllowNonBlankTapes checks that the tape path forwards
// Library.AllowNonBlankTapes from the run config into every Load activity call, so
// the Load phase can honour the operator's opt-out (issue #91).
func TestRunTapePathPassesAllowNonBlankTapes(t *testing.T) {
	t.Parallel()

	for _, allow := range []bool{false, true} {
		allow := allow
		t.Run(fmt.Sprintf("allow=%t", allow), func(t *testing.T) {
			t.Parallel()

			env := newTapePathEnv(t)

			var (
				mu   sync.Mutex
				seen []bool
			)

			env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
				func(_ context.Context, input LoadInput) ([]LoadedTape, error) {
					mu.Lock()

					seen = append(seen, input.AllowNonBlankTapes)
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

			env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
				[]byte("<ltfsindex></ltfsindex>"), nil)
			env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
				WriteHealth{}, nil)
			env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
				EjectResult{}, nil)

			cfg := tapePathConfig(1, 1, 1)
			cfg.Library.AllowNonBlankTapes = allow
			plan, staged, par2 := seededPlan(1, 1)

			env.ExecuteWorkflow(tapePathTestWorkflow, tapePathTestParams{
				Cfg: cfg, Plan: plan, Staged: staged, PAR2: par2,
			})

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())

			mu.Lock()
			defer mu.Unlock()

			require.NotEmpty(t, seen, "Load activity must be called at least once")

			for _, got := range seen {
				assert.Equal(t, allow, got, "Load must receive the configured AllowNonBlankTapes")
			}
		})
	}
}

// TestRunTapePathTeardownRunsOnCancellation verifies that when the workflow is
// cancelled mid-Write — while a WriteTree activity is still holding a live LTFS
// mount — the deferred TeardownSession activity is nonetheless dispatched to and
// executed on the data worker, so the session's mounts are released (issue #133
// AC1). Before the fix the teardown was dispatched on the cancelled session
// context, which the SDK fails immediately without ever scheduling the activity,
// leaking the mount. The workflow-cancel here reproduces that path, and the test
// fails (teardown never runs) against the pre-fix dispatch.
func TestRunTapePathTeardownRunsOnCancellation(t *testing.T) {
	t.Parallel()

	env := newTapePathEnv(t)

	var mu sync.Mutex

	var loadSetSizes []int

	mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)

	// WriteTree signals it is in flight, then blocks until its activity context
	// is cancelled — modelling a live mount held open when the operator cancels
	// the run mid-write. Its return arrives after cancellation, so writePhase
	// unwinds into the deferred teardown while the workflow is being cancelled.
	writeInFlight := make(chan struct{})

	var closeOnce sync.Once

	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, _ WriteTreeInput) error {
			closeOnce.Do(func() { close(writeInFlight) })
			<-ctx.Done()

			return ctx.Err()
		})
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		[]byte("<ltfsindex></ltfsindex>"), nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		WriteHealth{}, nil)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)

	// Spy on TeardownSession: recording the call proves the activity was
	// dispatched and executed on the (session-pinned) data worker after
	// cancellation — the exact behavior the pre-fix code skipped.
	var teardownCalled bool

	env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(
		func(context.Context, TeardownInput) error {
			mu.Lock()
			teardownCalled = true
			mu.Unlock()

			return nil
		})

	// Cancel once WriteTree is in flight (holding the mount). CancelWorkflow
	// posts onto the test env's callback channel, so it is safe to invoke from
	// this goroutine while ExecuteWorkflow drives the workflow.
	go func() {
		<-writeInFlight
		env.CancelWorkflow()
	}()

	plan, staged, par2 := seededPlan(1, 1)

	env.ExecuteWorkflow(tapePathTestWorkflow, tapePathTestParams{
		Cfg: tapePathConfig(1, 1, 1), Plan: plan, Staged: staged, PAR2: par2,
	})

	require.True(t, env.IsWorkflowCompleted())

	mu.Lock()
	defer mu.Unlock()

	assert.True(t, teardownCalled,
		"TeardownSession must run on the data worker after cancellation so the session's live mounts are released")
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
		func(_ context.Context, input EjectInput) (EjectResult, error) {
			mu.Lock()
			ejectCalls++

			for _, wt := range input.WrittenTapes {
				ejectedBarcodes = append(ejectedBarcodes, wt.Barcode)
			}
			mu.Unlock()

			// All tapes exported (no Remaining) → no operator pause.
			return EjectResult{}, nil
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

// TestRunTapePathStopsAfterSetFailure verifies the bounded blast radius when a
// tape in a drive-set fails its write: the run pauses for the operator and, if the
// operator aborts, no later set is loaded (issue #66 AC5, as amended by issue #92:
// a write failure now pauses for operator approval rather than failing the whole
// run outright — but the "no later set is loaded" bound is preserved).
func TestRunTapePathStopsAfterSetFailure(t *testing.T) {
	t.Parallel()

	const (
		drives = 1 // one physical tape per set, so failure lands in the first set
		tapes  = 3
		copies = 1
	)

	env := newTapePathEnv(t)
	env.RegisterActivity(&FailureActivities{})

	var mu sync.Mutex

	var loadSetSizes []int

	mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

	// The first (and only) tape in set 0 fails to format; the Write phase pauses,
	// and the operator aborts, so runTapePath returns before any later set loads.
	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		fmt.Errorf("simulated format failure"))
	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		[]byte("<ltfsindex></ltfsindex>"), nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		WriteHealth{}, nil)

	// AC2 (issue #133): teardown still runs on an ordinary write-phase failure
	// (no cancellation). Record the call so the guarantee is locked, not merely
	// mocked away.
	teardownCalls := 0

	env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(
		func(context.Context, TeardownInput) error {
			mu.Lock()
			teardownCalls++
			mu.Unlock()

			return nil
		})
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(nil)

	ejectCalls := 0

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ EjectInput) (EjectResult, error) {
			mu.Lock()
			ejectCalls++
			mu.Unlock()

			return EjectResult{}, nil
		})

	// The operator aborts the paused run rather than reloading fresh blanks.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorAbortSignal, nil)
	}, 30*time.Second)

	plan, staged, par2 := seededPlan(tapes, copies)

	env.ExecuteWorkflow(tapePathTestWorkflow, tapePathTestParams{
		Cfg:    tapePathConfig(drives, tapes, copies),
		Plan:   plan,
		Staged: staged,
		PAR2:   par2,
	})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "an aborted write failure must fail the run")
	assert.Contains(t, err.Error(), "aborted by operator", "the run ends in the aborted state")

	mu.Lock()
	defer mu.Unlock()

	assert.Len(t, loadSetSizes, 1, "only the failing set may be loaded; no later set is loaded")
	assert.Equal(t, 1, ejectCalls, "the failed set's tape is ejected so its drive frees and its slot empties")
	assert.Positive(t, teardownCalls, "teardown must still run on an ordinary write-phase failure (issue #133 AC2)")
}

// TestRunTapePathResumeNeverReformatsCompletedTapes covers issue #92 AC5 across
// drive-sets: with two sets, the first completes and one tape in the second fails.
// On resume only that failed tape is re-driven — the first set's tapes (a
// completed drive-set) and the second set's tape that succeeded are never
// re-formatted, so an already-written tape is never overwritten.
func TestRunTapePathResumeNeverReformatsCompletedTapes(t *testing.T) {
	t.Parallel()

	const (
		drives = 2
		tapes  = 4 // two drive-sets of two tapes each
		copies = 1
	)

	env := newTapePathEnv(t)
	env.RegisterActivity(&FailureActivities{})

	var (
		mu            sync.Mutex
		loadSetSizes  []int
		formatsByTape = map[tape.Barcode]int{}
	)

	mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

	// Barcodes are BC-<tapeIndex>-<copyIndex>; tape index 3 (in the second set)
	// fails to format on its first attempt, then succeeds on the resume retry.
	failed := tape.Barcode("BC-3-0")

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formatsByTape[input.Barcode]++

			if input.Barcode == failed && formatsByTape[input.Barcode] == 1 {
				return fmt.Errorf("simulated format failure")
			}

			return nil
		})

	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		[]byte("<ltfsindex></ltfsindex>"), nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		WriteHealth{}, nil)
	env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(nil)

	// The operator loads a fresh blank and resumes.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

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
	require.Len(t, written, tapes*copies, "every physical tape ends up written")

	mu.Lock()
	defer mu.Unlock()
	// Only the failed tape is ever re-formatted; every other tape — including the
	// whole first (completed) drive-set — is formatted exactly once (AC5).
	assert.Equal(t, 2, formatsByTape[failed], "the failed tape is re-formatted once on resume")

	for _, barcode := range []tape.Barcode{"BC-0-0", "BC-1-0", "BC-2-0"} {
		assert.Equal(t, 1, formatsByTape[barcode],
			"already-written tape %s must never be re-formatted", barcode)
	}
}
