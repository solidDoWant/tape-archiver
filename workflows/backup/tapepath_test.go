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

// tapePathTestParams seeds a runState for tapePathTestWorkflow.
type tapePathTestParams struct {
	Cfg    config.Config
	Plan   TapePlan
	Staged []StagedArchive
	PAR2   []PAR2Set
}

// tapePathTestWorkflow drives only runTapePath over a seeded plan.
func tapePathTestWorkflow(ctx workflow.Context, params tapePathTestParams) ([]WrittenTape, error) {
	state := &runState{plan: params.Plan, staged: params.Staged, par2: params.PAR2}

	failing := ""
	if err := runTapePath(ctx, params.Cfg, state, &failing); err != nil {
		return nil, fmt.Errorf("%s: %w", failing, err)
	}

	return state.written, nil
}

// newTapePathEnv registers the tape-path test workflow and activities, with the
// session worker Write needs.
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

// tapePathConfig returns a config with the given drives and enough blank slots
// for copyCount copies of tapeCount tapes.
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

// seededPlan builds tapeCount one-archive tapes plus matching staged and PAR2
// trees.
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

// mockLoadReturnsAssignments mocks Load to echo each assignment with a synthetic
// barcode/device, recording each set's size in setSizes (under mu).
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

// TestRunTapePathPassesAllowNonBlankTapes: Library.AllowNonBlankTapes reaches
// every Load call (issue #91).
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
			env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(WriteTreeResult{}, nil)
			env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
				"/stage/indexes/tape.xml", nil)
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

// TestRunTapePathTeardownRunsOnCancellation: cancelling mid-Write still runs
// TeardownSession on the data worker (issue #133 AC1). Dispatching it on the
// cancelled session context would never schedule it, leaking the mount.
func TestRunTapePathTeardownRunsOnCancellation(t *testing.T) {
	t.Parallel()

	env := newTapePathEnv(t)

	var mu sync.Mutex

	var loadSetSizes []int

	mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)

	// WriteTree blocks until cancelled, modelling a live mount at cancel time.
	writeInFlight := make(chan struct{})

	var closeOnce sync.Once

	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, _ WriteTreeInput) (WriteTreeResult, error) {
			closeOnce.Do(func() { close(writeInFlight) })
			<-ctx.Done()

			return WriteTreeResult{}, ctx.Err()
		})
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		"/stage/indexes/tape.xml", nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		WriteHealth{}, nil)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)

	var teardownCalled bool

	env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(
		func(context.Context, TeardownInput) error {
			mu.Lock()
			teardownCalled = true
			mu.Unlock()

			return nil
		})

	// CancelWorkflow is safe to call from this goroutine.
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

// TestRunTapePathMultipleDriveSets: every (tape, copy) is written and ejected
// with concurrency bounded by the drive count (issue #66 AC1-AC4).
func TestRunTapePathMultipleDriveSets(t *testing.T) {
	t.Parallel()

	const (
		drives = 2
		tapes  = 3
		copies = 3
	)

	env := newTapePathEnv(t)

	var mu sync.Mutex

	var loadSetSizes []int

	mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

	writeArchives := make(map[[2]int][]TapeWriteArchive)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input WriteTreeInput) (WriteTreeResult, error) {
			mu.Lock()
			writeArchives[[2]int{input.TapeIndex, input.CopyIndex}] = input.Archives
			mu.Unlock()

			return WriteTreeResult{}, nil
		})
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		"/stage/indexes/tape.xml", nil)
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

	assert.Len(t, ejectedBarcodes, tapes*copies, "every written tape must be ejected")
}

// TestRunTapePathStopsAfterSetFailure: after a write failure and operator abort,
// no later set is loaded (issue #66 AC5, amended by #92).
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

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		fmt.Errorf("simulated format failure"))
	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(WriteTreeResult{}, nil)
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		"/stage/indexes/tape.xml", nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		WriteHealth{}, nil)

	// Teardown still runs on an ordinary write failure (issue #133 AC2).
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

// TestRunTapePathFinalizeRetryBounded: a persistently failing FinalizeTape is
// retried a bounded number of times, then pauses as a write failure instead of
// retrying until the 24h session timeout (issue #152 AC4).
func TestRunTapePathFinalizeRetryBounded(t *testing.T) {
	t.Parallel()

	const (
		drives = 1
		tapes  = 1
		copies = 1
	)

	env := newTapePathEnv(t)
	env.RegisterActivity(&FailureActivities{})

	var (
		mu               sync.Mutex
		loadSetSizes     []int
		finalizeAttempts int
	)

	mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(WriteTreeResult{}, nil)

	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ FinalizeInput) (string, error) {
			mu.Lock()
			finalizeAttempts++
			mu.Unlock()

			return "", fmt.Errorf("unmount LTFS volume on /dev/sg0: persistent media error")
		})

	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		WriteHealth{}, nil)
	env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		EjectResult{}, nil)

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
	require.Error(t, env.GetWorkflowError(), "a persistent finalize failure must fail the run")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 3, finalizeAttempts,
		"FinalizeTape must be bounded to MaximumAttempts=3 so a persistent unmount failure surfaces promptly instead of retrying until the session timeout")
}

// TestRunTapePathTeardownRetryBounded: a wedged TeardownSession retries a bounded
// number of times so writePhase returns and the failure alert can fire, instead of
// hanging forever (issue #223 AC1/AC2). The written tape is still recorded.
func TestRunTapePathTeardownRetryBounded(t *testing.T) {
	t.Parallel()

	const (
		drives = 1
		tapes  = 1
		copies = 1
	)

	env := newTapePathEnv(t)

	var (
		mu               sync.Mutex
		loadSetSizes     []int
		teardownAttempts int
	)

	mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(WriteTreeResult{}, nil)
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		"/stage/indexes/tape.xml", nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		WriteHealth{}, nil)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		EjectResult{}, nil)

	// Unbounded retries would hang writePhase until the session timeout.
	env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(
		func(context.Context, TeardownInput) error {
			mu.Lock()
			teardownAttempts++
			mu.Unlock()

			return fmt.Errorf("simulated wedged unmount: teardown timed out")
		})

	plan, staged, par2 := seededPlan(tapes, copies)

	env.ExecuteWorkflow(tapePathTestWorkflow, tapePathTestParams{
		Cfg:    tapePathConfig(drives, tapes, copies),
		Plan:   plan,
		Staged: staged,
		PAR2:   par2,
	})

	require.True(t, env.IsWorkflowCompleted(),
		"the write phase must return within a bounded time even when teardown keeps failing")
	require.NoError(t, env.GetWorkflowError(),
		"a best-effort teardown failure must not fail the run or mask the phase result")

	var written []WrittenTape
	require.NoError(t, env.GetWorkflowResult(&written))
	assert.Len(t, written, 1, "the successfully written tape is still recorded despite the teardown failure")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 3, teardownAttempts,
		"TeardownSession must be bounded to MaximumAttempts=3 so a wedged unmount cannot retry until the session timeout and hang the workflow with no failure alert")
}

// TestRunTapePathWriteHealthRetryBounded: a hanging MeasureWriteHealth is tried
// once, so a finished write is not stalled (issue #223 AC3). Health is
// observational only (SPEC §14); the tape is still recorded.
func TestRunTapePathWriteHealthRetryBounded(t *testing.T) {
	t.Parallel()

	const (
		drives = 1
		tapes  = 1
		copies = 1
	)

	env := newTapePathEnv(t)

	var (
		mu             sync.Mutex
		loadSetSizes   []int
		healthAttempts int
	)

	mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(WriteTreeResult{}, nil)
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		"/stage/indexes/tape.xml", nil)

	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		func(context.Context, MeasureWriteHealthInput) (TapeMeasurement, error) {
			mu.Lock()
			healthAttempts++
			mu.Unlock()

			return TapeMeasurement{}, fmt.Errorf("simulated hung scrape: measure write health timed out")
		})

	env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		EjectResult{}, nil)

	plan, staged, par2 := seededPlan(tapes, copies)

	env.ExecuteWorkflow(tapePathTestWorkflow, tapePathTestParams{
		Cfg:    tapePathConfig(drives, tapes, copies),
		Plan:   plan,
		Staged: staged,
		PAR2:   par2,
	})

	require.True(t, env.IsWorkflowCompleted(),
		"the write phase must conclude within a bounded time even when the health scrape hangs every attempt")
	require.NoError(t, env.GetWorkflowError(),
		"an observational write-health failure must never fail the run")

	var written []WrittenTape
	require.NoError(t, env.GetWorkflowResult(&written))
	require.Len(t, written, 1, "the tape is still recorded as written when health cannot be measured")
	assert.Equal(t, WriteHealth{}, written[0].WriteHealth,
		"a tape whose health scrape hung is recorded unmeasured (zero-value health)")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, healthAttempts,
		"MeasureWriteHealth must be bounded to MaximumAttempts=1 so a hung scrape cannot retry until the session timeout and stall a finished write")
}

// TestRunTapePathResumeNeverReformatsCompletedTapes: on resume only the failed
// tape is re-driven, so a written tape is never re-formatted (issue #92 AC5).
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

	// Tape 3 (second set) fails its first format, then succeeds on resume.
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

	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(WriteTreeResult{}, nil)
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
		"/stage/indexes/tape.xml", nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
		WriteHealth{}, nil)
	env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(nil)

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

	assert.Equal(t, 2, formatsByTape[failed], "the failed tape is re-formatted once on resume")

	for _, barcode := range []tape.Barcode{"BC-0-0", "BC-1-0", "BC-2-0"} {
		assert.Equal(t, 1, formatsByTape[barcode],
			"already-written tape %s must never be re-formatted", barcode)
	}
}

// TestRunTapePathRejectsBadBarcodesBeforeMount: empty or duplicate barcodes fail
// the run, naming the offenders, before any format or mount (issue #170).
func TestRunTapePathRejectsBadBarcodesBeforeMount(t *testing.T) {
	t.Parallel()

	// badLoad echoes the assignments with the bad barcodes, by drive position.
	tests := []struct {
		name           string
		barcodes       []tape.Barcode
		wantSubstrings []string
	}{
		{
			name:           "empty barcodes collide on a shared mountpoint",
			barcodes:       []tape.Barcode{"", ""},
			wantSubstrings: []string{"write phase", "drive 0", "drive 1", "empty barcode"},
		},
		{
			name:           "duplicate barcodes collide on a shared mountpoint",
			barcodes:       []tape.Barcode{"BC-DUP", "BC-DUP"},
			wantSubstrings: []string{"write phase", "duplicate barcode", "BC-DUP", "[0 1]"},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			env := newTapePathEnv(t)

			var (
				mu          sync.Mutex
				formatCalls int
				writeCalls  int
			)

			env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
				func(_ context.Context, input LoadInput) ([]LoadedTape, error) {
					loaded := make([]LoadedTape, len(input.Tapes))
					for i, assignment := range input.Tapes {
						loaded[i] = LoadedTape{
							Barcode:    test.barcodes[i],
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
			env.OnActivity((&LoadActivities{}).ReturnToSlots, mock.Anything, mock.Anything).Return(nil)

			env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
				func(_ context.Context, _ FormatInput) error {
					mu.Lock()
					formatCalls++
					mu.Unlock()

					return nil
				})
			env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(
				func(_ context.Context, _ WriteTreeInput) (WriteTreeResult, error) {
					mu.Lock()
					writeCalls++
					mu.Unlock()

					return WriteTreeResult{}, nil
				})

			plan, staged, par2 := seededPlan(1, 2)

			env.ExecuteWorkflow(tapePathTestWorkflow, tapePathTestParams{
				Cfg:    tapePathConfig(2, 1, 2),
				Plan:   plan,
				Staged: staged,
				PAR2:   par2,
			})

			require.True(t, env.IsWorkflowCompleted())

			err := env.GetWorkflowError()
			require.Error(t, err, "a bad-barcode drive-set must fail the run")

			for _, want := range test.wantSubstrings {
				assert.ErrorContains(t, err, want)
			}

			mu.Lock()
			defer mu.Unlock()

			assert.Zero(t, formatCalls, "no tape may be formatted before barcode validation")
			assert.Zero(t, writeCalls, "no LTFS volume may be mounted/written before barcode validation")
		})
	}
}

// TestRunTapePathWriteHealthTimesStreamOnly: the elapsed MeasureWriteHealth
// receives is WriteTree's streaming-copy duration, not the window through
// FinalizeTape, whose mount and index write swamp a small write (issue #409).
func TestRunTapePathWriteHealthTimesStreamOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		written     WriteTreeResult
		wantElapsed time.Duration
	}{
		{name: "stream duration", written: WriteTreeResult{StreamDuration: 97 * time.Minute}, wantElapsed: 97 * time.Minute},
		// A WriteTree recorded before #409 decodes as the zero result.
		{name: "pre-#409 result", written: WriteTreeResult{}, wantElapsed: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			env := newTapePathEnv(t)

			var (
				mu           sync.Mutex
				loadSetSizes []int
				elapsed      []time.Duration
			)

			mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

			env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(test.written, nil)
			// A slow finalize models the unmount-time index write.
			env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return(
				func(context.Context, FinalizeInput) (string, error) {
					time.Sleep(50 * time.Millisecond)

					return "/stage/indexes/tape.xml", nil
				})
			env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
				func(_ context.Context, input MeasureWriteHealthInput) (TapeMeasurement, error) {
					mu.Lock()

					elapsed = append(elapsed, input.Elapsed)
					mu.Unlock()

					return TapeMeasurement{}, nil
				})
			env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)

			plan, staged, par2 := seededPlan(1, 1)

			env.ExecuteWorkflow(tapePathTestWorkflow, tapePathTestParams{
				Cfg: tapePathConfig(1, 1, 1), Plan: plan, Staged: staged, PAR2: par2,
			})

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())

			mu.Lock()
			defer mu.Unlock()

			assert.Equal(t, []time.Duration{test.wantElapsed}, elapsed,
				"write health must be timed by WriteTree's stream duration only")
		})
	}
}

// TestRunTapePathWriteHealthLabelsDryRun: MeasureWriteHealth learns whether the
// run is a dry run, so its gauges never overwrite a real tape's (#412).
func TestRunTapePathWriteHealthLabelsDryRun(t *testing.T) {
	t.Parallel()

	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dryRun=%t", dryRun), func(t *testing.T) {
			t.Parallel()

			env := newTapePathEnv(t)
			require.NoError(t, env.SetMemoOnStart(map[string]interface{}{MemoKeyDryRun: dryRun}))

			var (
				mu           sync.Mutex
				loadSetSizes []int
				labels       []bool
			)

			mockLoadReturnsAssignments(env, &mu, &loadSetSizes)

			env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(WriteTreeResult{}, nil)
			env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return("/stage/indexes/tape.xml", nil)
			env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(
				func(_ context.Context, input MeasureWriteHealthInput) (TapeMeasurement, error) {
					mu.Lock()

					labels = append(labels, input.DryRun)
					mu.Unlock()

					return TapeMeasurement{}, nil
				})
			env.OnActivity((&TeardownActivities{}).TeardownSession, mock.Anything, mock.Anything).Return(nil)
			env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)

			plan, staged, par2 := seededPlan(1, 1)

			env.ExecuteWorkflow(tapePathTestWorkflow, tapePathTestParams{
				Cfg: tapePathConfig(1, 1, 1), Plan: plan, Staged: staged, PAR2: par2,
			})

			require.True(t, env.IsWorkflowCompleted())
			require.NoError(t, env.GetWorkflowError())

			mu.Lock()
			defer mu.Unlock()

			assert.Equal(t, []bool{dryRun}, labels)
		})
	}
}
