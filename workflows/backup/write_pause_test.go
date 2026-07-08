package backup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// resumeWhilePauseAlertFires models an operator who reacts to a pause alert by
// sending OperatorResumeSignal in the gap between the pause-alert activity and the
// workflow task that begins the wait. That gap — opened by a control-worker
// restart/deploy or a task-queue backlog — is exactly the window in which a
// stale-signal drain run at wait entry would discard a legitimate resume
// (issue #216): the resume is buffered ahead of the task that arms the wait, so a
// drain at that task discards it. Delivering the resume as the alert activity
// starts (via SetOnActivityStartedListener) buffers it while the alert runs,
// reproducing that ordering deterministically: the test fails on a
// drain-at-wait-entry implementation (the run times out) and passes once the drain
// runs before the alert dispatch. It fires exactly once, on the named pause-alert
// activity's first start.
func resumeWhilePauseAlertFires(env *testsuite.TestWorkflowEnvironment, alertActivity string) {
	var once sync.Once

	// Deliver the resume as the alert activity starts: the signal is then buffered
	// while the alert runs, so it lands in history before the workflow task that
	// begins the wait. A drain relocated ahead of the alert dispatch has already
	// run, so the resume survives; a drain at wait entry runs after the alert and
	// discards it (the run then times out) — which is the #216 regression.
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name != alertActivity {
			return
		}

		once.Do(func() {
			env.SignalWorkflow(OperatorResumeSignal, nil)
		})
	})
}

// writePauseParams seeds the writePauseTestWorkflow. driveSet is a named slice of
// exported TapeAssignment, so it round-trips through the test env's data converter.
type writePauseParams struct {
	Cfg config.Config
	Set driveSet
}

// writePauseTestWorkflow drives runDriveSet for a single drive-set so its
// Load/Write-failure pause loop can be tested with mocked tape activities,
// signals, and the test env's time-skipping — without the full pipeline. It seeds
// a plan whose logical tapes carry no archives, so archivesForTape yields an empty
// (valid) tree and the mocked WriteTree copies nothing.
func writePauseTestWorkflow(ctx workflow.Context, params writePauseParams) error {
	state := &runState{plan: TapePlan{Copies: 2, Tapes: []PlannedTape{{}, {}}}}
	failing := ""

	return runDriveSet(ctx, params.Cfg, state, params.Set, &failing)
}

// twoDriveConfig is a two-drive library so a single drive-set holds two physical
// tapes — enough for a partial (one-tape) write failure. writeFailureWaitSeconds
// bounds the operator pause.
func twoDriveConfig(writeFailureWaitSeconds int) config.Config {
	return config.Config{
		Library: config.Library{
			Changer:                        "/dev/sch0",
			Drives:                         []string{"/dev/nst0", "/dev/nst1"},
			BlankSlots:                     []int{100, 101},
			TapeCapacityBytes:              2_500_000_000_000,
			WriteFailureWaitTimeoutSeconds: &writeFailureWaitSeconds,
		},
	}
}

// twoTapeSet is one drive-set of two physical tapes (the two copies of one logical
// tape), one per drive, drawn from slots 100 and 101. DriveIndex mirrors what
// planDriveSets stamps (the drive's config index), so the mocked Load pairs each
// tape to the same physical drive the real activity would (issue #137).
func twoTapeSet() driveSet {
	return driveSet{
		{Drive: "/dev/nst0", DriveIndex: 0, BlankSlot: 100, TapeIndex: 0, CopyIndex: 0},
		{Drive: "/dev/nst1", DriveIndex: 1, BlankSlot: 101, TapeIndex: 0, CopyIndex: 1},
	}
}

// sgForDrive maps a non-rewinding node to its SCSI generic node, mirroring the
// pairing the real Load activity resolves from the changer.
func sgForDrive(nst string) string {
	switch nst {
	case "/dev/nst0":
		return "/dev/sg0"
	case "/dev/nst1":
		return "/dev/sg1"
	default:
		return "/dev/sg9"
	}
}

// barcodeForSlot gives each blank slot a stable barcode so the mocked Load returns
// deterministic tapes across the original write and the resume retry.
func barcodeForSlot(slot int) tape.Barcode {
	return tape.Barcode(fmt.Sprintf("TA%04dL6", slot))
}

// loadedFromAssignment mirrors the real Load activity's identity-based contract:
// the tape lands on the drive the assignment names (STDevice = assignment.Drive)
// and its recorded DriveIndex is the assignment's config drive index — not its
// position in the set (issue #137).
func loadedFromAssignment(assignment TapeAssignment) LoadedTape {
	return LoadedTape{
		Barcode:    barcodeForSlot(assignment.BlankSlot),
		DriveIndex: assignment.DriveIndex,
		TapeIndex:  assignment.TapeIndex,
		CopyIndex:  assignment.CopyIndex,
		SourceSlot: assignment.BlankSlot,
		STDevice:   assignment.Drive,
		SGDevice:   sgForDrive(assignment.Drive),
	}
}

// mockLoadFromSet makes the Load activity synthesize LoadedTapes from its input
// assignments — so both the initial full set and the narrowed resume set load the
// tapes the assignments name, keyed by slot for stable barcodes/devices.
func mockLoadFromSet(env *testsuite.TestWorkflowEnvironment, _ config.Config) {
	env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input LoadInput) ([]LoadedTape, error) {
			loaded := make([]LoadedTape, len(input.Tapes))
			for i, assignment := range input.Tapes {
				loaded[i] = loadedFromAssignment(assignment)
			}

			return loaded, nil
		})
}

// newWritePauseEnv registers the Backup activities the write path dispatches, with
// the session worker enabled so writePhase's session succeeds. Tests override the
// specific activity behavior they exercise with OnActivity.
func newWritePauseEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(writePauseTestWorkflow)
	env.SetWorkerOptions(worker.Options{EnableSessionWorker: true})

	env.RegisterActivity(newLoadActivities())

	registry := newMountRegistry()
	env.RegisterActivity(newWriteActivities(registry, t.TempDir()))
	env.RegisterActivity(newTeardownActivities(registry))
	env.RegisterActivity(newWriteHealthActivities(nil))
	env.RegisterActivity(newEjectActivities())
	env.RegisterActivity(&FailureActivities{})

	return env
}

// expectHealthyWriteExceptFormat mocks WriteTree, FinalizeTape, and
// MeasureWriteHealth to succeed for every tape, leaving FormatTape for the caller
// to control which tapes fail.
func expectHealthyWriteExceptFormat(env *testsuite.TestWorkflowEnvironment) {
	env.OnActivity((&WriteActivities{}).WriteTree, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&WriteActivities{}).FinalizeTape, mock.Anything, mock.Anything).Return("/stage/indexes/tape.xml", nil)
	env.OnActivity((&WriteHealthActivities{}).MeasureWriteHealth, mock.Anything, mock.Anything).Return(WriteHealth{}, nil)
}

// TestWritePathPauseSignalResume covers AC1, AC2, and AC5: when one tape in a
// drive-set fails to write, the run ejects and keeps the tape that succeeded,
// pauses and alerts the operator, and on resume re-drives only the failed tape
// onto a fresh blank — the tape that already succeeded is never re-formatted.
func TestWritePathPauseSignalResume(t *testing.T) {
	cfg := twoDriveConfig(3600)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu           sync.Mutex
		formatsByDev = map[string]int{}
		ejectCounts  []int
		pauseAlerts  int
		loadInputs   []LoadInput
	)

	// /dev/sg1 (drive 1) fails to format on its first attempt, then succeeds on the
	// resume retry; /dev/sg0 (drive 0) always succeeds.
	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formatsByDev[input.Device]++

			if input.Device == "/dev/sg1" && formatsByDev[input.Device] == 1 {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	// Record every Load input so the resume retry set can be inspected: the
	// re-drive must target the failed tape's original physical drive (AC1) and
	// carry its config drive index (AC2), not drive 0 or the set position.
	env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input LoadInput) ([]LoadedTape, error) {
			mu.Lock()
			defer mu.Unlock()

			loadInputs = append(loadInputs, input)

			loaded := make([]LoadedTape, len(input.Tapes))
			for i, assignment := range input.Tapes {
				loaded[i] = loadedFromAssignment(assignment)
			}

			return loaded, nil
		})

	// Eject always exports everything (no I/O-full pause). Record how many tapes
	// each call ejects: the first ejects both the succeeded and the failed tape,
	// the second (after resume) ejects only the retried tape.
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input EjectInput) (EjectResult, error) {
			mu.Lock()
			defer mu.Unlock()

			ejectCounts = append(ejectCounts, len(input.WrittenTapes))

			return EjectResult{}, nil
		})

	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			assert.Equal(t, PhaseWrite, input.Phase, "alert names the Write phase")
			assert.Equal(t, []string{string(barcodeForSlot(101))}, input.AffectedTapes,
				"alert names only the failed tape")
			assert.Equal(t, []int{101}, input.ReloadSlots, "alert names the failed tape's slot to restock")

			return nil
		})

	// The operator swaps the suspect tape for a fresh blank and resumes.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, 1, pauseAlerts, "the operator is alerted exactly once")
	assert.Equal(t, 1, formatsByDev["/dev/sg0"], "the tape that succeeded is never re-formatted (AC5)")
	assert.Equal(t, 2, formatsByDev["/dev/sg1"], "the failed tape is re-formatted once on resume")
	assert.Equal(t, []int{2, 1}, ejectCounts, "first eject frees both tapes; resume ejects only the retried one")

	// The resume Load re-drives only the failed tape, and onto its original
	// physical drive: the retry set is one assignment naming drive 1's device node
	// and config index (issue #137 AC1/AC2), not drive 0 or the set-position drive.
	require.Len(t, loadInputs, 2, "one Load for the initial set, one for the resume retry")
	retry := loadInputs[1].Tapes
	require.Len(t, retry, 1, "the resume retry re-drives only the failed tape")
	assert.Equal(t, "/dev/nst1", retry[0].Drive, "retried on the failed tape's physical drive")
	assert.Equal(t, 1, retry[0].DriveIndex, "retried tape carries its config drive index (AC2)")
	assert.Equal(t, 101, retry[0].BlankSlot, "the fresh blank is reloaded into the failed tape's slot")
}

// TestWritePathResumeInAlertToWaitGapResumes covers issue #216 AC1: a resume the
// operator sends after a write-failure pause's alert has fired but before the
// workflow task that begins the wait executes (a control-worker restart/deploy or
// task-queue backlog) must resume the run, not be drained as stale. /dev/sg1 fails
// its first format and succeeds on the resume retry; the resume is delivered from
// the pause alert firing, so it is buffered ahead of the wait task. A drain
// at wait entry would discard it and the run would time out.
func TestWritePathResumeInAlertToWaitGapResumes(t *testing.T) {
	cfg := twoDriveConfig(300)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu           sync.Mutex
		formatsByDev = map[string]int{}
		pauseAlerts  int
	)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formatsByDev[input.Device]++

			if input.Device == "/dev/sg1" && formatsByDev[input.Device] == 1 {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	// The operator resumes in the alert-to-wait-entry gap.
	resumeWhilePauseAlertFires(env, "NotifyWritePathPause")

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "a resume in the alert-to-wait gap must resume the run, not be drained")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, pauseAlerts, "the operator is alerted exactly once")
	assert.Equal(t, 2, formatsByDev["/dev/sg1"], "the failed tape is re-formatted on the honored resume")
}

// TestWritePathStaleResumeSignalDoesNotSatisfyLaterPause covers issue #154 AC1 for
// the write path: a surplus resume buffered during one pause must not instantly
// satisfy a later pause. /dev/sg1 fails to format on its first two attempts; during
// the first write-failure pause the operator sends TWO resume signals. The first
// resumes and re-drives the failed tape, which fails again and pauses a second time;
// with the drain the surplus signal is discarded at that pause's entry, so it waits
// and — no fresh action arriving — times out. Without the drain the stale signal
// would resume it and the third format attempt would succeed.
func TestWritePathStaleResumeSignalDoesNotSatisfyLaterPause(t *testing.T) {
	cfg := twoDriveConfig(300)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu           sync.Mutex
		formatsByDev = map[string]int{}
		pauseAlerts  int
	)

	// /dev/sg1 fails its first two format attempts (so the run pauses twice); it
	// would only succeed on a third attempt that a drained stale signal must prevent.
	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formatsByDev[input.Device]++

			if input.Device == "/dev/sg1" && formatsByDev[input.Device] <= 2 {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, _ WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			pauseAlerts++

			return nil
		})

	// During the first write-failure pause the operator double-sends resume. The
	// first clears that pause; the surplus must not leak forward to the second.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the surplus resume must not satisfy the second pause; it times out")
	assert.Contains(t, err.Error(), "did not resume or abort")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 2, pauseAlerts, "both write-failure pauses alert the operator")
	assert.Equal(t, 2, formatsByDev["/dev/sg1"], "the failed tape is retried once; the drained signal blocks a third attempt")
}

// TestWritePathPauseAbort covers AC3: an operator abort ends the run in a defined,
// reported state with no further tapes written (no resume retry).
func TestWritePathPauseAbort(t *testing.T) {
	cfg := twoDriveConfig(3600)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	var (
		mu      sync.Mutex
		formats int
	)

	// /dev/sg1 always fails to format, so a resume (if any) would fail again — the
	// abort must stop the run instead of looping.
	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			mu.Lock()
			defer mu.Unlock()

			formats++

			if input.Device == "/dev/sg1" {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorAbortSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "an aborted run ends with an error")
	assert.Contains(t, err.Error(), "aborted by operator")

	mu.Lock()
	defer mu.Unlock()
	// Two format attempts only: the original set (sg0 ok, sg1 fails). No third
	// attempt, because abort stops the run rather than retrying the failed tape.
	assert.Equal(t, 2, formats, "no tape is written after abort")
}

// TestWritePathPauseTimeout covers AC4: when the operator neither resumes nor
// aborts, the wait elapses and the run fails in its defined paused state, reported.
func TestWritePathPauseTimeout(t *testing.T) {
	cfg := twoDriveConfig(100)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)

	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input FormatInput) error {
			if input.Device == "/dev/sg1" {
				return errors.New("mkltfs: drive reported a hard write error")
			}

			return nil
		})

	mockLoadFromSet(env, cfg)
	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(nil)

	// No signal is ever sent: the 100s wait elapses.
	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())

	err := env.GetWorkflowError()
	require.Error(t, err, "the run fails when the operator never responds")
	assert.Contains(t, err.Error(), "did not resume or abort")
}

// TestWritePathLoadFailurePauseResume covers AC1 for a Load failure: the phase
// pauses at set granularity and, on resume, retries the whole set.
func TestWritePathLoadFailurePauseResume(t *testing.T) {
	cfg := twoDriveConfig(3600)
	env := newWritePauseEnv(t)
	expectHealthyWriteExceptFormat(env)
	env.OnActivity((&WriteActivities{}).FormatTape, mock.Anything, mock.Anything).Return(nil)

	var (
		mu         sync.Mutex
		loadCalls  int
		loadAlerts int
	)

	// Load fails the first time, then succeeds on resume.
	env.OnActivity((&LoadActivities{}).Load, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input LoadInput) ([]LoadedTape, error) {
			mu.Lock()
			defer mu.Unlock()

			loadCalls++

			if loadCalls == 1 {
				return nil, errors.New("move medium: source slot empty")
			}

			loaded := make([]LoadedTape, len(input.Tapes))
			for i, assignment := range input.Tapes {
				loaded[i] = loadedFromAssignment(assignment)
			}

			return loaded, nil
		})

	env.OnActivity((&EjectActivities{}).Eject, mock.Anything, mock.Anything).Return(EjectResult{}, nil)
	env.OnActivity((&FailureActivities{}).NotifyWritePathPause, mock.Anything, mock.Anything).Return(
		func(_ context.Context, input WritePathPauseInput) error {
			mu.Lock()
			defer mu.Unlock()

			loadAlerts++

			assert.Equal(t, PhaseLoad, input.Phase, "alert names the Load phase")
			assert.Empty(t, input.AffectedTapes, "a Load failure has no loaded tapes to name")
			assert.Equal(t, []int{100, 101}, input.ReloadSlots, "alert names the whole set's slots")

			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(OperatorResumeSignal, nil)
	}, 30*time.Second)

	env.ExecuteWorkflow(writePauseTestWorkflow, writePauseParams{Cfg: cfg, Set: twoTapeSet()})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, 1, loadAlerts, "the operator is alerted once for the Load failure")
	assert.Equal(t, 2, loadCalls, "Load runs once, pauses, then retries the whole set on resume")
}

// TestEjectProjectionDropsIndexAndHealth pins the Part-A fix for issue #221: the
// Eject payload must not carry the captured LTFS index (nor the write-health),
// only the identity and slot fields Eject actually reads. This keeps EjectInput
// bounded regardless of run size — so a large run's post-write Eject phase never
// fails on the Temporal payload-size limit.
func TestEjectProjectionDropsIndexAndHealth(t *testing.T) {
	t.Parallel()

	written := []WrittenTape{
		{
			Barcode:      tape.Barcode("TAPE01L6"),
			DriveIndex:   0,
			TapeIndex:    1,
			CopyIndex:    0,
			SourceSlot:   4,
			IndexXMLPath: "/stage/run-1/indexes/TAPE01L6.xml",
			WriteHealth:  WriteHealth{Repositions: 7},
		},
		{
			Barcode:      tape.Barcode("TAPE02L6"),
			DriveIndex:   1,
			TapeIndex:    1,
			CopyIndex:    1,
			SourceSlot:   5,
			IndexXMLPath: "/stage/run-1/indexes/TAPE02L6.xml",
			WriteHealth:  WriteHealth{Repositions: 9},
		},
	}

	projected := ejectProjection(written)

	require.Len(t, projected, 2)

	for i, got := range projected {
		assert.Empty(t, got.IndexXMLPath, "eject payload must not carry the staged index path (entry %d)", i)
		assert.Zero(t, got.WriteHealth, "eject payload must not carry write-health (entry %d)", i)
		// Identity and slot fields Eject reads are preserved.
		assert.Equal(t, written[i].Barcode, got.Barcode)
		assert.Equal(t, written[i].DriveIndex, got.DriveIndex)
		assert.Equal(t, written[i].TapeIndex, got.TapeIndex)
		assert.Equal(t, written[i].CopyIndex, got.CopyIndex)
		assert.Equal(t, written[i].SourceSlot, got.SourceSlot)
	}

	// The projection must be a fresh slice: mutating it never touches the run record.
	projected[0].Barcode = tape.Barcode("MUTATED")
	assert.Equal(t, tape.Barcode("TAPE01L6"), written[0].Barcode, "projection must not alias the input")
}
