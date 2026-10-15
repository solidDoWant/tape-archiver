package runsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/runsubmit"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// fakeHistoryIterator fakes the Temporal SDK's history iterator.
type fakeHistoryIterator struct {
	events []*historypb.HistoryEvent
	err    error
	index  int
}

func (f *fakeHistoryIterator) HasNext() bool {
	if f.err != nil {
		return f.index == 0
	}

	return f.index < len(f.events)
}

func (f *fakeHistoryIterator) Next() (*historypb.HistoryEvent, error) {
	if f.err != nil {
		f.index++

		return nil, f.err
	}

	event := f.events[f.index]
	f.index++

	return event, nil
}

// GetWorkflowHistory serves per-run histories via historyFunc.
func (f *fakeTemporalClient) GetWorkflowHistory(_ context.Context, _, runID string, _ bool, _ enumspb.HistoryEventFilterType) client.HistoryEventIterator {
	if f.historyFunc != nil {
		return f.historyFunc(runID)
	}

	return &fakeHistoryIterator{}
}

// mustEncode uses the same default converter decodePayloads decodes with.
func mustEncode(t *testing.T, value interface{}) *commonpb.Payloads {
	t.Helper()

	payloads, err := converter.GetDefaultDataConverter().ToPayloads(value)
	require.NoError(t, err)

	return payloads
}

// eventBuilder assembles a synthetic history with monotonic IDs and times.
type eventBuilder struct {
	events []*historypb.HistoryEvent
	nextID int64
	now    time.Time
}

func newEventBuilder() *eventBuilder {
	return &eventBuilder{nextID: 1, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (b *eventBuilder) tick() time.Time {
	b.now = b.now.Add(time.Minute)

	return b.now
}

func (b *eventBuilder) id() int64 {
	id := b.nextID
	b.nextID++

	return id
}

func (b *eventBuilder) started(t *testing.T, input interface{}) {
	t.Helper()

	b.events = append(b.events, &historypb.HistoryEvent{
		EventId:   b.id(),
		EventTime: timestamppb.New(b.tick()),
		EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED,
		Attributes: &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{
			WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{
				Input: mustEncode(t, input),
			},
		},
	})
}

// startedDryRun records a start event with the dry-run memo, as runsubmit does.
func (b *eventBuilder) startedDryRun(t *testing.T, input interface{}, dryRun bool) {
	t.Helper()

	memoPayload, err := converter.GetDefaultDataConverter().ToPayload(dryRun)
	require.NoError(t, err)

	b.events = append(b.events, &historypb.HistoryEvent{
		EventId:   b.id(),
		EventTime: timestamppb.New(b.tick()),
		EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED,
		Attributes: &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{
			WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{
				Input: mustEncode(t, input),
				Memo:  &commonpb.Memo{Fields: map[string]*commonpb.Payload{runsubmit.MemoKeyDryRun: memoPayload}},
			},
		},
	})
}

// scheduled returns the EventId so the caller can complete/fail it later.
func (b *eventBuilder) scheduled(t *testing.T, name string, input interface{}) int64 {
	t.Helper()

	id := b.id()
	b.events = append(b.events, &historypb.HistoryEvent{
		EventId:   id,
		EventTime: timestamppb.New(b.tick()),
		EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED,
		Attributes: &historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{
			ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{
				ActivityId:   strconv.FormatInt(id, 10),
				ActivityType: &commonpb.ActivityType{Name: name},
				Input:        mustEncode(t, input),
			},
		},
	})

	return id
}

func (b *eventBuilder) completed(t *testing.T, scheduledID int64, result interface{}) {
	t.Helper()

	b.events = append(b.events, &historypb.HistoryEvent{
		EventId:   b.id(),
		EventTime: timestamppb.New(b.tick()),
		EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED,
		Attributes: &historypb.HistoryEvent_ActivityTaskCompletedEventAttributes{
			ActivityTaskCompletedEventAttributes: &historypb.ActivityTaskCompletedEventAttributes{
				ScheduledEventId: scheduledID,
				Result:           mustEncode(t, result),
			},
		},
	})
}

func (b *eventBuilder) failed(scheduledID int64, message string) *eventBuilder {
	b.events = append(b.events, &historypb.HistoryEvent{
		EventId:   b.id(),
		EventTime: timestamppb.New(b.tick()),
		EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED,
		Attributes: &historypb.HistoryEvent_ActivityTaskFailedEventAttributes{
			ActivityTaskFailedEventAttributes: &historypb.ActivityTaskFailedEventAttributes{
				ScheduledEventId: scheduledID,
				Failure:          &failurepb.Failure{Message: message},
			},
		},
	})

	return b
}

func (b *eventBuilder) runCompleted() {
	b.events = append(b.events, &historypb.HistoryEvent{
		EventId:   b.id(),
		EventTime: timestamppb.New(b.tick()),
		EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED,
		Attributes: &historypb.HistoryEvent_WorkflowExecutionCompletedEventAttributes{
			WorkflowExecutionCompletedEventAttributes: &historypb.WorkflowExecutionCompletedEventAttributes{},
		},
	})
}

func (b *eventBuilder) runFailed(message string) {
	b.events = append(b.events, &historypb.HistoryEvent{
		EventId:   b.id(),
		EventTime: timestamppb.New(b.tick()),
		EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED,
		Attributes: &historypb.HistoryEvent_WorkflowExecutionFailedEventAttributes{
			WorkflowExecutionFailedEventAttributes: &historypb.WorkflowExecutionFailedEventAttributes{
				Failure: &failurepb.Failure{Message: message},
			},
		},
	})
}

// --- fixture: a full, successful run through all 11 phases ---

var testConfig = config.Config{
	Sources: []config.Source{{ZFSPath: &config.ZFSPathSource{Name: "pool/archive@snap"}}},
	Copies:  1,
	Library: config.Library{
		Changer: "/dev/sch0", Drives: []string{"/dev/nst0"}, BlankSlots: []int{1},
		TapeCapacityBytes: 2_500_000_000_000,
	},
	Redundancy: config.Redundancy{TargetPercentage: floatPtr(10)},
	Encryption: config.Encryption{Recipients: []string{"age1pq1examplerecipient"}, Identity: "AGE-SECRET-KEY-PQ-1EXAMPLE"},
	Delivery:   config.Delivery{WebhookURL: "https://discord.example/webhook"},
}

func floatPtr(f float64) *float64 { return &f }

// buildSuccessfulRunHistory covers all 11 phases plus one failed tape write
// retried on a fresh tape, exercising both written and failed outcomes.
func buildSuccessfulRunHistory(t *testing.T) []*historypb.HistoryEvent {
	t.Helper()

	b := newEventBuilder()
	b.started(t, testConfig)

	resolveK8s := b.scheduled(t, "ResolveK8sSources", testConfig)
	b.completed(t, resolveK8s, []backup.ResolvedArchive{})

	resolveCheck := b.scheduled(t, "ResolveAndCheck", backup.ResolveDataInput{Config: testConfig})
	// 5 TB / 800 = 6.25 GB, rounded to 6 GiB slices.
	b.completed(t, resolveCheck, []backup.ResolvedArchive{{SourceIndex: 0, Label: "archive-0", EstimatedBytes: 5_000_000_000_000}})

	hold := b.scheduled(t, "HoldSnapshots", nil)
	b.completed(t, hold, nil)

	prepare := b.scheduled(t, "PrepareArchives", nil)
	b.completed(t, prepare, []backup.StagedArchive{{SourceIndex: 0, SizeBytes: 1024}})

	pack := b.scheduled(t, "Pack", nil)
	b.completed(t, pack, backup.TapePlan{Copies: 1, Tapes: []backup.PlannedTape{{}}})

	par2 := b.scheduled(t, "GeneratePAR2", nil)
	b.completed(t, par2, []backup.PAR2Set{{SourceIndex: 0}})

	verify := b.scheduled(t, "Verify", backup.VerifyInput{
		Archives: []backup.StagedArchive{{Slices: []backup.StagedSlice{{Path: "a"}, {Path: "b"}}}},
		PAR2:     []backup.PAR2Set{{Files: []backup.StagedSlice{{Path: "c"}}}},
	})
	b.completed(t, verify, backup.VerifiedPlan{})

	// One tape fails at WriteTree; a second Load writes a fresh tape.
	load1 := b.scheduled(t, "Load", nil)
	b.completed(t, load1, []backup.LoadedTape{
		{Barcode: "FAILTAPE01", TapeIndex: 0, CopyIndex: 0, DriveIndex: 0, SourceSlot: 1},
	})

	format1 := b.scheduled(t, "FormatTape", struct{ Barcode string }{"FAILTAPE01"})
	b.completed(t, format1, nil)

	write1 := b.scheduled(t, "WriteTree", struct{ Barcode string }{"FAILTAPE01"})
	b.failed(write1, "drive 0: write tree: medium error")

	writePause := b.scheduled(t, "NotifyWritePathPause", backup.WritePathPauseInput{Phase: backup.PhaseWrite})
	b.completed(t, writePause, nil)

	load2 := b.scheduled(t, "Load", nil)
	b.completed(t, load2, []backup.LoadedTape{
		{Barcode: "GOODTAPE01", TapeIndex: 0, CopyIndex: 0, DriveIndex: 0, SourceSlot: 1},
	})

	format2 := b.scheduled(t, "FormatTape", struct{ Barcode string }{"GOODTAPE01"})
	b.completed(t, format2, nil)

	write2 := b.scheduled(t, "WriteTree", struct{ Barcode string }{"GOODTAPE01"})
	b.completed(t, write2, nil)

	finalize2 := b.scheduled(t, "FinalizeTape", struct{ Barcode string }{"GOODTAPE01"})
	b.completed(t, finalize2, "/staging/GOODTAPE01/index.xml")

	// Encode the real TapeMeasurement wrapper, not a bare WriteHealth, or the
	// fixture and decoder can be wrong together (issue #338).
	health2 := b.scheduled(t, "MeasureWriteHealth", struct{ Barcode string }{"GOODTAPE01"})
	b.completed(t, health2, backup.TapeMeasurement{
		Health: backup.WriteHealth{
			Measured: true, ThroughputMBps: 140, FloorMBps: 50, FloorKnown: true,
			RepositionsMeasured: true,
		},
		MediaGeneration: "LTO-6",
	})

	eject := b.scheduled(t, "Eject", nil)
	b.completed(t, eject, backup.EjectResult{InIOStation: []tape.Barcode{"GOODTAPE01"}})

	report := b.scheduled(t, "BuildReport", nil)
	b.completed(t, report, backup.ReportOutput{ReportPath: "/staging/report.pdf"})

	// Burn is disabled in testConfig, so it schedules no activity.

	deliver := b.scheduled(t, "Deliver", nil)
	b.completed(t, deliver, nil)

	release := b.scheduled(t, "ReleaseSnapshots", nil)
	b.completed(t, release, nil)

	b.runCompleted()

	return b.events
}

func TestFetchRunHistory(t *testing.T) {
	t.Run("classifies a not-found run", func(t *testing.T) {
		fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{err: serviceerror.NewNotFound("workflow execution not found")}
		}}

		_, err := fetchRunHistory(t.Context(), fake, "missing")
		require.Error(t, err)
		assert.Equal(t, http.StatusNotFound, statusForTemporalError(err))
	})

	t.Run("parses a successful run's activities and start input", func(t *testing.T) {
		fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
		}}

		history, err := fetchRunHistory(t.Context(), fake, "run-1")
		require.NoError(t, err)

		assert.True(t, history.Closed)
		assert.True(t, history.Succeeded)
		assert.NotNil(t, history.StartInput)
		assert.NotEmpty(t, history.Activities)

		var cfg config.Config
		require.NoError(t, decodePayloads(history.StartInput, &cfg))
		assert.Equal(t, testConfig.Sources[0].ZFSPath.Name, cfg.Sources[0].ZFSPath.Name)
	})
}

func TestBuildPhaseTimeline(t *testing.T) {
	t.Run("a fully successful run marks every phase completed with facts", func(t *testing.T) {
		history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
		}}, "run-1")
		require.NoError(t, err)

		outcomes := deriveTapeOutcomes(history.Activities)
		phases := buildPhaseTimeline(history, outcomes)

		require.Len(t, phases, 11)

		names := make([]string, len(phases))
		for i, phase := range phases {
			names[i] = phase.Name
			assert.Equal(t, PhaseCompleted, phase.Status, "phase %s", phase.Name)

			// No-op Burn has no time window.
			if phase.Name == backup.PhaseBurn {
				assert.Nil(t, phase.StartTime, "no-op Burn has no start time")
				assert.Nil(t, phase.EndTime, "no-op Burn has no end time")

				continue
			}

			assert.NotNil(t, phase.StartTime, "phase %s start time", phase.Name)
			assert.NotNil(t, phase.EndTime, "phase %s end time", phase.Name)
		}

		assert.Equal(t, []string{
			backup.PhaseResolve, backup.PhasePrepare, backup.PhasePack, backup.PhaseGeneratePAR2,
			backup.PhaseVerify, backup.PhaseLoad, backup.PhaseWrite, backup.PhaseEject,
			backup.PhaseReport, backup.PhaseBurn, backup.PhaseDeliver,
		}, names)

		byName := make(map[string]PhaseInfo, len(phases))
		for _, phase := range phases {
			byName[phase.Name] = phase
		}

		assertFactValue(t, byName[backup.PhaseResolve].Facts, "archives", "1")
		assertFactValue(t, byName[backup.PhaseResolve].Facts, "sliceSize", "6 GiB")
		assertFactValue(t, byName[backup.PhasePrepare].Facts, "archivesStaged", "1")
		assertFactValue(t, byName[backup.PhasePack].Facts, "logicalTapes", "1")
		assertFactValue(t, byName[backup.PhasePack].Facts, "copies", "1")
		assertFactValue(t, byName[backup.PhaseGeneratePAR2].Facts, "recoverySets", "1")
		assertFactValue(t, byName[backup.PhaseVerify].Facts, "filesVerified", "3/3")
		assertFactValue(t, byName[backup.PhaseLoad].Facts, "tapesLoaded", "2")
		assertFactValue(t, byName[backup.PhaseWrite].Facts, "tapesWritten", "1")
		assertFactValue(t, byName[backup.PhaseWrite].Facts, "tapesFailed", "1")
		assertFactValue(t, byName[backup.PhaseReport].Facts, "reportBuilt", "yes")
		assertFactValue(t, byName[backup.PhaseBurn].Facts, "opticalBurn", "disabled")
		assertFactValue(t, byName[backup.PhaseDeliver].Facts, "delivered", "yes")
	})

	t.Run("a run still in progress marks the active phase and leaves the rest pending", func(t *testing.T) {
		b := newEventBuilder()
		b.started(t, testConfig)

		resolveK8s := b.scheduled(t, "ResolveK8sSources", testConfig)
		b.completed(t, resolveK8s, []backup.ResolvedArchive{})
		resolveCheck := b.scheduled(t, "ResolveAndCheck", nil)
		b.completed(t, resolveCheck, []backup.ResolvedArchive{{SourceIndex: 0}})

		hold := b.scheduled(t, "HoldSnapshots", nil)
		b.completed(t, hold, nil)

		// Prepare is in flight: scheduled, no terminal event yet.
		b.scheduled(t, "PrepareArchives", nil)

		history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: b.events}
		}}, "run-2")
		require.NoError(t, err)

		phases := buildPhaseTimeline(history, nil)

		byName := make(map[string]PhaseInfo, len(phases))
		for _, phase := range phases {
			byName[phase.Name] = phase
		}

		assert.Equal(t, PhaseCompleted, byName[backup.PhaseResolve].Status)
		assert.Equal(t, PhaseActive, byName[backup.PhasePrepare].Status)
		assert.NotNil(t, byName[backup.PhasePrepare].StartTime)
		assert.Nil(t, byName[backup.PhasePrepare].EndTime)
		assert.Equal(t, PhasePending, byName[backup.PhasePack].Status)
		assert.Equal(t, PhasePending, byName[backup.PhaseDeliver].Status)
	})

	t.Run("a run that fails mid-pipeline marks the failing phase failed and later phases pending", func(t *testing.T) {
		b := newEventBuilder()
		b.started(t, testConfig)

		resolveK8s := b.scheduled(t, "ResolveK8sSources", testConfig)
		b.completed(t, resolveK8s, []backup.ResolvedArchive{})
		resolveCheck := b.scheduled(t, "ResolveAndCheck", nil)
		b.completed(t, resolveCheck, []backup.ResolvedArchive{{SourceIndex: 0}})
		hold := b.scheduled(t, "HoldSnapshots", nil)
		b.completed(t, hold, nil)

		prepare := b.scheduled(t, "PrepareArchives", nil)
		b.failed(prepare, "tar: snapshot vanished")

		notify := b.scheduled(t, "NotifyFailure", backup.FailureInput{
			RunID: "run-3", Phase: backup.PhasePrepare, ErrorSummary: "phase Prepare: tar: snapshot vanished",
		})
		b.completed(t, notify, nil)

		b.runFailed("phase Prepare: tar: snapshot vanished")

		history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: b.events}
		}}, "run-3")
		require.NoError(t, err)

		phases := buildPhaseTimeline(history, nil)

		byName := make(map[string]PhaseInfo, len(phases))
		for _, phase := range phases {
			byName[phase.Name] = phase
		}

		assert.Equal(t, PhaseCompleted, byName[backup.PhaseResolve].Status)
		assert.Equal(t, PhaseFailed, byName[backup.PhasePrepare].Status)
		assert.Contains(t, byName[backup.PhasePrepare].Error, "tar: snapshot vanished")
		assert.Equal(t, PhasePending, byName[backup.PhasePack].Status)
		assert.Equal(t, PhasePending, byName[backup.PhaseDeliver].Status)
	})

	t.Run("a Hold failure surfaces as a Resolve failure (design decision, issue #273)", func(t *testing.T) {
		b := newEventBuilder()
		b.started(t, testConfig)

		resolveK8s := b.scheduled(t, "ResolveK8sSources", testConfig)
		b.completed(t, resolveK8s, []backup.ResolvedArchive{})
		resolveCheck := b.scheduled(t, "ResolveAndCheck", nil)
		b.completed(t, resolveCheck, []backup.ResolvedArchive{{SourceIndex: 0}})

		hold := b.scheduled(t, "HoldSnapshots", nil)
		b.failed(hold, "zfs hold: dataset busy")

		notify := b.scheduled(t, "NotifyFailure", backup.FailureInput{
			RunID: "run-4", Phase: backup.PhaseHold, ErrorSummary: "phase Hold: zfs hold: dataset busy",
		})
		b.completed(t, notify, nil)

		b.runFailed("phase Hold: zfs hold: dataset busy")

		history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: b.events}
		}}, "run-4")
		require.NoError(t, err)

		phases := buildPhaseTimeline(history, nil)

		byName := make(map[string]PhaseInfo, len(phases))
		for _, phase := range phases {
			byName[phase.Name] = phase
		}

		assert.Equal(t, PhaseFailed, byName[backup.PhaseResolve].Status)
		assert.Contains(t, byName[backup.PhaseResolve].Error, "zfs hold: dataset busy")
	})

	t.Run("an older-code-version run with no NotifyFailure input falls back to the terminal message", func(t *testing.T) {
		b := newEventBuilder()
		b.started(t, testConfig)

		verify := b.scheduled(t, "Verify", backup.VerifyInput{})
		b.failed(verify, "checksum mismatch")
		// No NotifyFailure: an older workflow version or a stub.
		b.runFailed("phase Verify: checksum mismatch")

		history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: b.events}
		}}, "run-5")
		require.NoError(t, err)

		assert.Equal(t, backup.PhaseVerify, history.FailingPhase)
	})

	t.Run("an older NotifyFailure with no Phase still recovers the phase from the terminal message", func(t *testing.T) {
		b := newEventBuilder()
		b.started(t, testConfig)

		verify := b.scheduled(t, "Verify", backup.VerifyInput{})
		b.failed(verify, "checksum mismatch")

		// Older NotifyFailure without Phase: recover it from the terminal
		// message.
		notify := b.scheduled(t, "NotifyFailure", backup.FailureInput{
			RunID: "run-7", ErrorSummary: "phase Verify: checksum mismatch",
		})
		b.completed(t, notify, nil)

		b.runFailed("phase Verify: checksum mismatch")

		history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: b.events}
		}}, "run-7")
		require.NoError(t, err)

		assert.Equal(t, backup.PhaseVerify, history.FailingPhase)
		assert.Equal(t, "phase Verify: checksum mismatch", history.FailingSummary)
	})

	t.Run("a second drive-set's Write failure is failed even though set 1's Eject already ran", func(t *testing.T) {
		// Interleaved tape path (SPEC §4.3): set 1 completes Load/Write/Eject,
		// set 2 fails in Write. Set 1's Eject must not mask the Write failure,
		// and Eject reads pending.
		b := newEventBuilder()
		b.started(t, testConfig)

		resolveK8s := b.scheduled(t, "ResolveK8sSources", testConfig)
		b.completed(t, resolveK8s, []backup.ResolvedArchive{})
		resolveCheck := b.scheduled(t, "ResolveAndCheck", nil)
		b.completed(t, resolveCheck, []backup.ResolvedArchive{{SourceIndex: 0}})
		hold := b.scheduled(t, "HoldSnapshots", nil)
		b.completed(t, hold, nil)

		prepare := b.scheduled(t, "PrepareArchives", nil)
		b.completed(t, prepare, []backup.StagedArchive{{SourceIndex: 0, SizeBytes: 1024}})

		pack := b.scheduled(t, "Pack", nil)
		b.completed(t, pack, backup.TapePlan{Copies: 2, Tapes: []backup.PlannedTape{{}}})

		par2 := b.scheduled(t, "GeneratePAR2", nil)
		b.completed(t, par2, []backup.PAR2Set{{SourceIndex: 0}})

		verify := b.scheduled(t, "Verify", backup.VerifyInput{})
		b.completed(t, verify, backup.VerifiedPlan{})

		// Drive-set 1.
		load1 := b.scheduled(t, "Load", nil)
		b.completed(t, load1, []backup.LoadedTape{
			{Barcode: "SET1TAPE01", TapeIndex: 0, CopyIndex: 0, DriveIndex: 0, SourceSlot: 1},
		})

		format1 := b.scheduled(t, "FormatTape", struct{ Barcode string }{"SET1TAPE01"})
		b.completed(t, format1, nil)

		write1 := b.scheduled(t, "WriteTree", struct{ Barcode string }{"SET1TAPE01"})
		b.completed(t, write1, nil)

		finalize1 := b.scheduled(t, "FinalizeTape", struct{ Barcode string }{"SET1TAPE01"})
		b.completed(t, finalize1, "/staging/SET1TAPE01/index.xml")

		eject1 := b.scheduled(t, "Eject", nil)
		b.completed(t, eject1, backup.EjectResult{InIOStation: []tape.Barcode{"SET1TAPE01"}})

		// Drive-set 2: WriteTree fails and the run fails in Write.
		load2 := b.scheduled(t, "Load", nil)
		b.completed(t, load2, []backup.LoadedTape{
			{Barcode: "SET2TAPE01", TapeIndex: 0, CopyIndex: 1, DriveIndex: 0, SourceSlot: 2},
		})

		format2 := b.scheduled(t, "FormatTape", struct{ Barcode string }{"SET2TAPE01"})
		b.completed(t, format2, nil)

		write2 := b.scheduled(t, "WriteTree", struct{ Barcode string }{"SET2TAPE01"})
		b.failed(write2, "drive 0: write tree: medium error")

		pause := b.scheduled(t, "NotifyWritePathPause", backup.WritePathPauseInput{Phase: backup.PhaseWrite})
		b.completed(t, pause, nil)

		notify := b.scheduled(t, "NotifyFailure", backup.FailureInput{
			RunID: "run-6", Phase: backup.PhaseWrite, ErrorSummary: "phase Write: run aborted by operator",
		})
		b.completed(t, notify, nil)

		b.runFailed("phase Write: run aborted by operator")

		history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: b.events}
		}}, "run-6")
		require.NoError(t, err)

		outcomes := deriveTapeOutcomes(history.Activities)
		phases := buildPhaseTimeline(history, outcomes)

		byName := make(map[string]PhaseInfo, len(phases))
		for _, phase := range phases {
			byName[phase.Name] = phase
		}

		// Every phase before the tape path completed normally.
		for _, name := range []string{
			backup.PhaseResolve, backup.PhasePrepare, backup.PhasePack,
			backup.PhaseGeneratePAR2, backup.PhaseVerify, backup.PhaseLoad,
		} {
			assert.Equal(t, PhaseCompleted, byName[name].Status, "phase %s", name)
		}

		assert.Equal(t, PhaseFailed, byName[backup.PhaseWrite].Status)
		assert.Contains(t, byName[backup.PhaseWrite].Error, "aborted by operator")

		assert.Equal(t, PhasePending, byName[backup.PhaseEject].Status)

		for _, name := range []string{backup.PhaseReport, backup.PhaseBurn, backup.PhaseDeliver} {
			assert.Equal(t, PhasePending, byName[name].Status, "phase %s", name)
		}

		byBarcode := make(map[string]TapeOutcome, len(outcomes))
		for _, outcome := range outcomes {
			byBarcode[outcome.Barcode] = outcome
		}

		assert.Equal(t, tapeOutcomeWritten, byBarcode["SET1TAPE01"].Result)
		assert.Equal(t, tapeOutcomeFailed, byBarcode["SET2TAPE01"].Result)
	})

	t.Run("an open run mid-tape-path keeps set 1's Eject active, never completed", func(t *testing.T) {
		// Set 2's WriteTree in flight: Write is the frontier, and Eject stays
		// active as part of the running tape path.
		b := newEventBuilder()
		b.started(t, testConfig)

		verify := b.scheduled(t, "Verify", backup.VerifyInput{})
		b.completed(t, verify, backup.VerifiedPlan{})

		load1 := b.scheduled(t, "Load", nil)
		b.completed(t, load1, []backup.LoadedTape{
			{Barcode: "SET1TAPE01", TapeIndex: 0, CopyIndex: 0, DriveIndex: 0, SourceSlot: 1},
		})

		write1 := b.scheduled(t, "WriteTree", struct{ Barcode string }{"SET1TAPE01"})
		b.completed(t, write1, nil)

		eject1 := b.scheduled(t, "Eject", nil)
		b.completed(t, eject1, backup.EjectResult{InIOStation: []tape.Barcode{"SET1TAPE01"}})

		load2 := b.scheduled(t, "Load", nil)
		b.completed(t, load2, []backup.LoadedTape{
			{Barcode: "SET2TAPE01", TapeIndex: 0, CopyIndex: 1, DriveIndex: 0, SourceSlot: 2},
		})

		b.scheduled(t, "WriteTree", struct{ Barcode string }{"SET2TAPE01"})

		history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: b.events}
		}}, "run-7")
		require.NoError(t, err)

		phases := buildPhaseTimeline(history, deriveTapeOutcomes(history.Activities))

		byName := make(map[string]PhaseInfo, len(phases))
		for _, phase := range phases {
			byName[phase.Name] = phase
		}

		assert.Equal(t, PhaseCompleted, byName[backup.PhaseVerify].Status)
		assert.Equal(t, PhaseCompleted, byName[backup.PhaseLoad].Status)
		assert.Equal(t, PhaseActive, byName[backup.PhaseWrite].Status)
		assert.Equal(t, PhaseActive, byName[backup.PhaseEject].Status,
			"set 1's partial Eject activity keeps the phase active on an open run, never prematurely completed")
		assert.Equal(t, PhasePending, byName[backup.PhaseReport].Status)
	})

	t.Run("a write-failure operator pause marks only Write active, not the already-completed Eject", func(t *testing.T) {
		// Issue #331: a write-failure pause ejects then pauses on Write. While
		// paused only the frontier is active, so Eject must not spin too.
		b := newEventBuilder()
		b.started(t, testConfig)

		verify := b.scheduled(t, "Verify", backup.VerifyInput{})
		b.completed(t, verify, backup.VerifiedPlan{})

		load := b.scheduled(t, "Load", nil)
		b.completed(t, load, []backup.LoadedTape{
			{Barcode: "TAPE01", TapeIndex: 0, CopyIndex: 0, DriveIndex: 0, SourceSlot: 1},
		})

		format := b.scheduled(t, "FormatTape", struct{ Barcode string }{"TAPE01"})
		b.completed(t, format, nil)

		write := b.scheduled(t, "WriteTree", struct{ Barcode string }{"TAPE01"})
		b.failed(write, "drive 0: write tree: medium error")

		eject := b.scheduled(t, "Eject", nil)
		b.completed(t, eject, backup.EjectResult{InIOStation: []tape.Barcode{"TAPE01"}})

		pause := b.scheduled(t, "NotifyWritePathPause", backup.WritePathPauseInput{Phase: backup.PhaseWrite})
		b.completed(t, pause, nil)

		history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: b.events}
		}}, "run-8")
		require.NoError(t, err)

		phases := buildPhaseTimeline(history, deriveTapeOutcomes(history.Activities))

		byName := make(map[string]PhaseInfo, len(phases))
		for _, phase := range phases {
			byName[phase.Name] = phase
		}

		assert.Equal(t, PhaseCompleted, byName[backup.PhaseVerify].Status)
		assert.Equal(t, PhaseCompleted, byName[backup.PhaseLoad].Status)

		write0 := byName[backup.PhaseWrite]
		assert.Equal(t, PhaseActive, write0.Status)
		assert.NotNil(t, write0.StartTime)
		assert.Nil(t, write0.EndTime)

		assert.Equal(t, PhasePending, byName[backup.PhaseEject].Status,
			"a completed Eject must not read active alongside the paused Write frontier")

		assert.Equal(t, PhasePending, byName[backup.PhaseReport].Status)
	})
}

func assertFactValue(t *testing.T, facts []PhaseFact, key, want string) {
	t.Helper()

	for _, fact := range facts {
		if fact.Key == key {
			assert.Equal(t, want, fact.Value, "fact %s", key)

			return
		}
	}

	t.Fatalf("fact %q not found among %+v", key, facts)
}

func TestDeriveTapeOutcomes(t *testing.T) {
	history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
	}}, "run-1")
	require.NoError(t, err)

	outcomes := deriveTapeOutcomes(history.Activities)
	require.Len(t, outcomes, 2)

	// The failed attempt and its retry share (TapeIndex, CopyIndex); the
	// barcode tiebreak keeps the order deterministic.
	require.Equal(t, outcomes[0].TapeIndex, outcomes[1].TapeIndex, "the retry reused the same logical tape index")
	require.Equal(t, outcomes[0].CopyIndex, outcomes[1].CopyIndex, "the retry reused the same copy index")
	assert.Equal(t, "FAILTAPE01", outcomes[0].Barcode, "barcode tiebreak orders tied outcomes deterministically")
	assert.Equal(t, "GOODTAPE01", outcomes[1].Barcode)

	byBarcode := make(map[string]TapeOutcome, len(outcomes))
	for _, outcome := range outcomes {
		byBarcode[outcome.Barcode] = outcome
	}

	failedTape := byBarcode["FAILTAPE01"]
	assert.Equal(t, tapeOutcomeFailed, failedTape.Result)
	assert.Contains(t, failedTape.Error, "medium error")
	assert.Equal(t, 0, failedTape.TapeIndex)
	assert.Equal(t, 1, failedTape.Slot)

	writtenTape := byBarcode["GOODTAPE01"]
	assert.Equal(t, tapeOutcomeWritten, writtenTape.Result)
	require.NotNil(t, writtenTape.WriteHealth)
	assert.True(t, writtenTape.WriteHealth.Measured, "the fixture's write-health was measured")
	assert.InDelta(t, 140, writtenTape.WriteHealth.ThroughputMBps, 0.001)
	assert.True(t, writtenTape.WriteHealth.FloorKnown)
}

// TestDeriveTapeOutcomesUnmeasuredWriteHealth: an unmeasured tape must be
// distinguishable from one measured unhealthy.
func TestDeriveTapeOutcomesUnmeasuredWriteHealth(t *testing.T) {
	b := newEventBuilder()
	b.started(t, testConfig)

	load := b.scheduled(t, "Load", nil)
	b.completed(t, load, []backup.LoadedTape{
		{Barcode: "UNMEASURED01", TapeIndex: 0, CopyIndex: 0, DriveIndex: 0, SourceSlot: 1},
	})

	format := b.scheduled(t, "FormatTape", struct{ Barcode string }{"UNMEASURED01"})
	b.completed(t, format, nil)

	write := b.scheduled(t, "WriteTree", struct{ Barcode string }{"UNMEASURED01"})
	b.completed(t, write, nil)

	finalize := b.scheduled(t, "FinalizeTape", struct{ Barcode string }{"UNMEASURED01"})
	b.completed(t, finalize, "/staging/UNMEASURED01/index.xml")

	// What session.go records when measurement fails.
	health := b.scheduled(t, "MeasureWriteHealth", struct{ Barcode string }{"UNMEASURED01"})
	b.completed(t, health, backup.TapeMeasurement{})

	b.runCompleted()

	history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: b.events}
	}}, "run-unmeasured")
	require.NoError(t, err)

	outcomes := deriveTapeOutcomes(history.Activities)
	require.Len(t, outcomes, 1)

	outcome := outcomes[0]
	assert.Equal(t, tapeOutcomeWritten, outcome.Result)
	require.NotNil(t, outcome.WriteHealth)
	assert.False(t, outcome.WriteHealth.Measured, "an untaken measurement must report measured=false")
	assert.False(t, outcome.WriteHealth.Healthy, "an unmeasured tape is never certified healthy")
}

// TestDeriveTapeOutcomesUnmeasurableThroughput: a write too small to judge is
// projected as not measurable (no rate), while a measurement recorded before
// #409 (no such field) projects as before.
func TestDeriveTapeOutcomesUnmeasurableThroughput(t *testing.T) {
	tests := []struct {
		name             string
		health           backup.WriteHealth
		wantUnmeasurable bool
		wantThroughput   float64
	}{
		{
			name: "too little data",
			health: backup.WriteHealth{
				Measured: true, ThroughputUnmeasurable: true, FloorMBps: 40, FloorKnown: true, RepositionsMeasured: true,
			},
			wantUnmeasurable: true,
		},
		{
			name: "recorded before #409",
			health: backup.WriteHealth{
				Measured: true, ThroughputMBps: 0.026, FloorMBps: 40, FloorKnown: true, BelowFloor: true, RepositionsMeasured: true,
			},
			wantThroughput: 0.026,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b := newEventBuilder()
			b.started(t, testConfig)

			load := b.scheduled(t, "Load", nil)
			b.completed(t, load, []backup.LoadedTape{{Barcode: "SMALLTAPE1", SourceSlot: 1}})

			format := b.scheduled(t, "FormatTape", struct{ Barcode string }{"SMALLTAPE1"})
			b.completed(t, format, nil)

			write := b.scheduled(t, "WriteTree", struct{ Barcode string }{"SMALLTAPE1"})
			b.completed(t, write, nil)

			finalize := b.scheduled(t, "FinalizeTape", struct{ Barcode string }{"SMALLTAPE1"})
			b.completed(t, finalize, "/staging/SMALLTAPE1/index.xml")

			health := b.scheduled(t, "MeasureWriteHealth", struct{ Barcode string }{"SMALLTAPE1"})
			b.completed(t, health, backup.TapeMeasurement{Health: test.health})

			b.runCompleted()

			history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
				return &fakeHistoryIterator{events: b.events}
			}}, "run-small")
			require.NoError(t, err)

			outcomes := deriveTapeOutcomes(history.Activities)
			require.Len(t, outcomes, 1)
			require.NotNil(t, outcomes[0].WriteHealth)

			info := outcomes[0].WriteHealth
			assert.Equal(t, test.wantUnmeasurable, info.ThroughputUnmeasurable)
			assert.InDelta(t, test.wantThroughput, info.ThroughputMBps, 0.0001)
			assert.Equal(t, test.health.BelowFloor, info.BelowFloor)
			assert.Equal(t, test.wantUnmeasurable, info.Healthy,
				"unmeasurable throughput alone never makes a tape unhealthy")

			encoded, err := json.Marshal(info)
			require.NoError(t, err)
			assert.Contains(t, string(encoded), fmt.Sprintf(`"throughputUnmeasurable":%t`, test.wantUnmeasurable))

			metric := toDriveMetric(outcomes[0], driveSample{})
			assert.Equal(t, test.wantUnmeasurable, metric.ThroughputUnmeasurable,
				"the live drive panel must learn the throughput was not measurable")
		})
	}
}

func TestGetRunPhasesHandler(t *testing.T) {
	fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
	}}
	handler := newMux(newHandler(fake, emptyEnv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/phases", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunPhasesResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, "run-1", body.RunID)
	assert.Len(t, body.Phases, 11)
}

func TestGetRunConfigHandler(t *testing.T) {
	fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
	}}
	handler := newMux(newHandler(fake, emptyEnv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/config", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunConfigResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, testConfig.Sources[0].ZFSPath.Name, body.Config.Sources[0].ZFSPath.Name)
	assert.Equal(t, testConfig.Encryption.Recipients, body.Config.Encryption.Recipients)
	assert.Equal(t, redactedSecret, body.Config.Encryption.Identity, "the age private identity must never leave the server")
	assert.Equal(t, redactedSecret, body.Config.Delivery.WebhookURL,
		"the Discord webhook URL embeds its auth token and must never leave the server")
}

func TestGetRunConfigHandlerDryRun(t *testing.T) {
	// The dry-run memo lets a restart preload keep the dry-run toggle.
	for _, dryRun := range []bool{true, false} {
		builder := newEventBuilder()
		builder.startedDryRun(t, testConfig, dryRun)

		fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: builder.events}
		}}
		handler := newMux(newHandler(fake, emptyEnv))

		recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/config", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body RunConfigResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
		assert.Equal(t, dryRun, body.DryRun)
	}
}

func TestGetRunTapesHandler(t *testing.T) {
	fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
	}}
	handler := newMux(newHandler(fake, emptyEnv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/tapes", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunTapesResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Len(t, body.Tapes, 2)
}

func TestListTapesHandler(t *testing.T) {
	t.Run("aggregates tapes across runs and degrades a run whose history is gone", func(t *testing.T) {
		start1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		start2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

		fake := &fakeTemporalClient{
			listResponse: &workflowservice.ListWorkflowExecutionsResponse{
				Executions: []*workflowpb.WorkflowExecutionInfo{
					executionInfo("run-good", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start1, &start1),
					executionInfo("run-gone", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start2, &start2),
				},
			},
			// run-gone gets the NotFound a retention-expired history returns.
			historyFunc: func(runID string) client.HistoryEventIterator {
				if runID == "run-good" {
					return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
				}

				return &fakeHistoryIterator{err: serviceerror.NewNotFound("workflow execution not found")}
			},
		}

		handler := newMux(newHandler(fake, emptyEnv))

		recorder := doJSON(t, handler, http.MethodGet, "/api/tapes", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body AggregateTapesResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

		assert.Len(t, body.Tapes, 2, "the reconstructable run's two tapes must still be listed")

		for _, tapeOutcome := range body.Tapes {
			assert.Equal(t, "run-good", tapeOutcome.RunID, "every tape must be attributed back to its run")
		}

		require.Len(t, body.RunErrors, 1, "the unreconstructable run must degrade, not fail the whole listing")
		assert.Equal(t, "run-gone", body.RunErrors[0].RunID)
	})

	t.Run("flags dry-run tapes from the run's memo so a virtual mhvtl barcode is never mistaken for physical media", func(t *testing.T) {
		start1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		start2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

		dryRunPayload, err := converter.GetDefaultDataConverter().ToPayload(true)
		require.NoError(t, err)

		dryRunExecution := executionInfo("run-dryrun", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start2, &start2)
		dryRunExecution.Memo = &commonpb.Memo{Fields: map[string]*commonpb.Payload{runsubmit.MemoKeyDryRun: dryRunPayload}}

		fake := &fakeTemporalClient{
			listResponse: &workflowservice.ListWorkflowExecutionsResponse{
				Executions: []*workflowpb.WorkflowExecutionInfo{
					// No dry-run memo: reads as production.
					executionInfo("run-prod", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start1, &start1),
					dryRunExecution,
				},
			},
			historyFunc: func(string) client.HistoryEventIterator {
				return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
			},
		}

		handler := newMux(newHandler(fake, emptyEnv))

		recorder := doJSON(t, handler, http.MethodGet, "/api/tapes", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body AggregateTapesResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

		byRun := map[string]bool{}
		for _, tapeOutcome := range body.Tapes {
			byRun[tapeOutcome.RunID] = tapeOutcome.DryRun
		}

		require.Contains(t, byRun, "run-dryrun")
		require.Contains(t, byRun, "run-prod")
		assert.True(t, byRun["run-dryrun"], "a tape from a dry-run run must be flagged dryRun")
		assert.False(t, byRun["run-prod"], "a tape from a production run must not be flagged dryRun")
	})

	t.Run("masks an upstream-fault error in a per-run RunError rather than leaking raw Temporal detail", func(t *testing.T) {
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		fake := &fakeTemporalClient{
			listResponse: &workflowservice.ListWorkflowExecutionsResponse{
				Executions: []*workflowpb.WorkflowExecutionInfo{
					executionInfo("run-flaky", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, &start),
				},
			},
			// Raw text carries an internal endpoint that must not leak.
			historyFunc: func(string) client.HistoryEventIterator {
				return &fakeHistoryIterator{
					err: serviceerror.NewUnavailable("connection error: desc = transport: dial tcp temporal-frontend.internal:7233: connect: connection refused"),
				}
			},
		}

		handler := newMux(newHandler(fake, emptyEnv))

		recorder := doJSON(t, handler, http.MethodGet, "/api/tapes", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body AggregateTapesResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

		require.Len(t, body.RunErrors, 1)
		assert.Equal(t, "run-flaky", body.RunErrors[0].RunID)
		assert.Equal(t, http.StatusText(http.StatusBadGateway), body.RunErrors[0].Error, "an upstream fault must be masked, not echoed verbatim")
		assert.NotContains(t, body.RunErrors[0].Error, "7233", "raw upstream endpoint detail must not leak to the client")
	})

	t.Run("limit bounds the reconstruction to the newest runs", func(t *testing.T) {
		older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		newer := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

		var fetched []string

		fake := &fakeTemporalClient{
			listResponse: &workflowservice.ListWorkflowExecutionsResponse{
				Executions: []*workflowpb.WorkflowExecutionInfo{
					// Oldest-first: the endpoint must sort before limiting.
					executionInfo("run-older", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, older, &older),
					executionInfo("run-newer", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, newer, &newer),
				},
			},
			historyFunc: func(runID string) client.HistoryEventIterator {
				fetched = append(fetched, runID)

				return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
			},
		}

		handler := newMux(newHandler(fake, emptyEnv))

		recorder := doJSON(t, handler, http.MethodGet, "/api/tapes?limit=1", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body AggregateTapesResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

		assert.Equal(t, []string{"run-newer"}, fetched, "only the newest run's history may be fetched under limit=1")

		for _, tapeOutcome := range body.Tapes {
			assert.Equal(t, "run-newer", tapeOutcome.RunID)
		}
	})

	t.Run("an invalid limit is a 400", func(t *testing.T) {
		handler := newMux(newHandler(&fakeTemporalClient{}, emptyEnv))

		for _, raw := range []string{"0", "-3", "abc"} {
			recorder := doJSON(t, handler, http.MethodGet, "/api/tapes?limit="+raw, nil)
			assert.Equal(t, http.StatusBadRequest, recorder.Code, "limit=%s", raw)
		}
	})

	t.Run("no runs yet serializes tapes as [] not null", func(t *testing.T) {
		// Must be [] not null (the client maps over it); check the raw body
		// since Unmarshal treats both alike.
		fake := &fakeTemporalClient{listResponse: &workflowservice.ListWorkflowExecutionsResponse{}}
		handler := newMux(newHandler(fake, emptyEnv))

		recorder := doJSON(t, handler, http.MethodGet, "/api/tapes", nil)
		require.Equal(t, http.StatusOK, recorder.Code)
		assert.Contains(t, recorder.Body.String(), `"tapes":[]`)
		assert.NotContains(t, recorder.Body.String(), `"tapes":null`)
	})

	t.Run("a canceled request surfaces a real status, not a degraded 200 of context errors", func(t *testing.T) {
		start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		fake := &fakeTemporalClient{
			listResponse: &workflowservice.ListWorkflowExecutionsResponse{
				Executions: []*workflowpb.WorkflowExecutionInfo{
					executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, &start),
				},
			},
			historyFunc: func(string) client.HistoryEventIterator {
				return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
			},
		}

		handler := newMux(newHandler(fake, emptyEnv))

		// A canceled request must not be reported as a 200 degraded listing.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		request := httptest.NewRequest(http.MethodGet, "/api/tapes", nil).WithContext(ctx)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		assert.Equal(t, statusClientClosedRequest, recorder.Code, "a global cancel must not be reported as a successful listing")
		assert.NotContains(t, recorder.Body.String(), `"tapes"`, "the degraded-listing body must not be emitted for a global failure")
	})
}

func emptyEnv(string) string { return "" }

// TestHistoryEndpointErrorClassification: aged-out history is 410, unknown run
// 404, malformed ID 400 (issue #273).
func TestHistoryEndpointErrorClassification(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	notFoundHistory := func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{err: serviceerror.NewNotFound("workflow execution not found")}
	}

	tests := []struct {
		name       string
		client     *fakeTemporalClient
		runID      string
		wantStatus int
	}{
		{
			name: "aged out of retention but still in visibility is 410",
			client: &fakeTemporalClient{
				historyFunc: notFoundHistory,
				listResponse: &workflowservice.ListWorkflowExecutionsResponse{
					Executions: []*workflowpb.WorkflowExecutionInfo{
						executionInfo("aged-run", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, &start),
					},
				},
			},
			runID:      "aged-run",
			wantStatus: http.StatusGone,
		},
		{
			name: "never a real execution is 404",
			client: &fakeTemporalClient{
				historyFunc:  notFoundHistory,
				listResponse: &workflowservice.ListWorkflowExecutionsResponse{},
			},
			runID:      "never-existed",
			wantStatus: http.StatusNotFound,
		},
		{
			name: "malformed run ID is 400",
			client: &fakeTemporalClient{
				historyFunc: func(string) client.HistoryEventIterator {
					return &fakeHistoryIterator{err: serviceerror.NewInvalidArgument("Invalid RunId.")}
				},
			},
			runID:      "not-a-uuid",
			wantStatus: http.StatusBadRequest,
		},
	}

	endpoints := []string{"phases", "config", "tapes"}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newMux(newHandler(test.client, emptyEnv))

			for _, endpoint := range endpoints {
				recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+test.runID+"/"+endpoint, nil)
				assert.Equal(t, test.wantStatus, recorder.Code, "endpoint %s", endpoint)
			}
		})
	}
}

// TestGetRunConfigForeignWorkflow: an undecodable start input (a stub sharing
// the WorkflowID) degrades to a per-run error, not a 500.
func TestGetRunConfigForeignWorkflow(t *testing.T) {
	b := newEventBuilder()
	// Not a config.Config at all.
	b.started(t, []string{"not", "a", "config"})
	b.runCompleted()

	fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: b.events}
	}}
	handler := newMux(newHandler(fake, emptyEnv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/stub-run/config", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, recorder.Code)
}

// TestGetRunPhasesForeignWorkflow: a stub run yields a well-formed timeline,
// not an error, so it cannot break aggregate views (issue #273).
func TestGetRunPhasesForeignWorkflow(t *testing.T) {
	b := newEventBuilder()
	b.started(t, "some foreign input")

	unknown := b.scheduled(t, "SomeForeignActivity", nil)
	b.completed(t, unknown, nil)
	b.runCompleted()

	fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: b.events}
	}}
	handler := newMux(newHandler(fake, emptyEnv))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/stub-run/phases", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunPhasesResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body.Phases, 11)

	for _, phase := range body.Phases {
		// Every phase reads as a no-op completion, with no fabricated times.
		assert.Equal(t, PhaseCompleted, phase.Status, "phase %s", phase.Name)
		assert.Nil(t, phase.StartTime, "phase %s must not invent a start time", phase.Name)
	}
}
