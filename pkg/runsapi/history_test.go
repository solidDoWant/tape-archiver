package runsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// fakeHistoryIterator is a minimal client.HistoryEventIterator standing in
// for the real SDK type, mirroring fakeEncodedValue/fakeTemporalClient in
// runsapi_test.go's "never mock the component under test" pattern: this
// fakes a Temporal SDK dependency, not runsapi itself.
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

// GetWorkflowHistory implements the TemporalClient method history.go's
// fetchRunHistory calls, routing through fakeTemporalClient.historyFunc.
func (f *fakeTemporalClient) GetWorkflowHistory(context.Context, string, string, bool, enumspb.HistoryEventFilterType) client.HistoryEventIterator {
	if f.historyFunc != nil {
		return f.historyFunc("")
	}

	return &fakeHistoryIterator{}
}

// historyRunID routes fakeTemporalClient.historyFunc by runID, since the real
// signature (matched above) does not thread it through to GetWorkflowHistory
// callers in a way this fake can see per-call; tests that need per-run
// behavior (the aggregate listTapes tests) instead build one
// fakeTemporalClient whose historyFunc dispatches on a fixed table the test
// already knows the runIDs for.
func historyRunID(_ context.Context) string { return "" }

// mustEncode encodes value into *commonpb.Payloads using Temporal's default
// data converter — the exact converter decodePayloads (history.go) decodes
// with, so a test round-trips through the real encoding.
func mustEncode(t *testing.T, value interface{}) *commonpb.Payloads {
	t.Helper()

	payloads, err := converter.GetDefaultDataConverter().ToPayloads(value)
	require.NoError(t, err)

	return payloads
}

// eventBuilder incrementally assembles a synthetic workflow history for
// buildPhaseTimeline/deriveTapeOutcomes/getRunConfig tests, assigning
// monotonic EventIds and EventTimes the way a real Temporal server would.
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

// scheduled appends an ACTIVITY_TASK_SCHEDULED event and returns its
// EventId, so the caller can complete/fail it later.
func (b *eventBuilder) scheduled(t *testing.T, name string, input interface{}) int64 {
	t.Helper()

	id := b.id()
	b.events = append(b.events, &historypb.HistoryEvent{
		EventId:   id,
		EventTime: timestamppb.New(b.tick()),
		EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED,
		Attributes: &historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{
			ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{
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

// runCompleted appends WORKFLOW_EXECUTION_COMPLETED.
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

// runFailed appends WORKFLOW_EXECUTION_FAILED with message.
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
	Redundancy: config.Redundancy{TargetPercentage: floatPtr(10), SliceSizeBytes: 1 << 20},
	Encryption: config.Encryption{Recipients: []string{"age1pq1examplerecipient"}, Identity: "AGE-SECRET-KEY-PQ-1EXAMPLE"},
	Delivery:   config.Delivery{WebhookURL: "https://discord.example/webhook"},
}

func floatPtr(f float64) *float64 { return &f }

// buildSuccessfulRunHistory returns a synthetic complete-and-successful run
// exercising every one of the 11 phases plus one tape write failure that gets
// retried and succeeds (Load's second call), so both deriveTapeOutcomes'
// "written" and "failed" outcomes are exercised in one fixture.
func buildSuccessfulRunHistory(t *testing.T) []*historypb.HistoryEvent {
	t.Helper()

	b := newEventBuilder()
	b.started(t, testConfig)

	resolveK8s := b.scheduled(t, "ResolveK8sSources", testConfig)
	b.completed(t, resolveK8s, []backup.ResolvedArchive{})

	resolveCheck := b.scheduled(t, "ResolveAndCheck", backup.ResolveDataInput{Config: testConfig})
	b.completed(t, resolveCheck, []backup.ResolvedArchive{{SourceIndex: 0, Label: "archive-0"}})

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

	// First drive-set: one tape fails at WriteTree (a Load/Write-failure
	// pause the operator resumes), then a second Load call loads a *fresh*
	// blank tape (a different barcode) that writes successfully.
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

	health2 := b.scheduled(t, "MeasureWriteHealth", struct{ Barcode string }{"GOODTAPE01"})
	b.completed(t, health2, backup.WriteHealth{
		Measured: true, ThroughputMBps: 140, FloorMBps: 50, FloorKnown: true,
		RepositionsMeasured: true,
	})

	eject := b.scheduled(t, "Eject", nil)
	b.completed(t, eject, backup.EjectResult{InIOStation: []backup.LoadedTape{}[:0:0]})
	// EjectResult.InIOStation is []tape.Barcode; encode directly below to
	// avoid an import cycle concern (there is none, but keep the fixture
	// simple and explicit about the type actually used).

	report := b.scheduled(t, "BuildReport", nil)
	b.completed(t, report, backup.ReportOutput{ReportPath: "/staging/report.pdf"})

	// Optical burning is disabled for this fixture (testConfig has no
	// opticalBurn section), so burnPhase is a true no-op: zero activity.

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
		// No NotifyFailure activity in this fixture at all — an older
		// workflow code version, or a foreign/stub workflow.
		b.runFailed("phase Verify: checksum mismatch")

		history, err := fetchRunHistory(t.Context(), &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
			return &fakeHistoryIterator{events: b.events}
		}}, "run-5")
		require.NoError(t, err)

		assert.Equal(t, backup.PhaseVerify, history.FailingPhase)
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
	assert.InDelta(t, 140, writtenTape.WriteHealth.ThroughputMBps, 0.001)
	assert.True(t, writtenTape.WriteHealth.FloorKnown)
}

// --- HTTP handler tests ---

func TestGetRunPhasesHandler(t *testing.T) {
	fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
	}}
	handler := newMux(newHandler(fake, envLookup(nil)))

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
	handler := newMux(newHandler(fake, envLookup(nil)))

	recorder := doJSON(t, handler, http.MethodGet, "/api/runs/run-1/config", nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var body RunConfigResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, testConfig.Sources[0].ZFSPath.Name, body.Config.Sources[0].ZFSPath.Name)
	assert.Equal(t, testConfig.Encryption.Recipients, body.Config.Encryption.Recipients)
	assert.Equal(t, redactedSecret, body.Config.Encryption.Identity, "the age private identity must never leave the server")
}

func TestGetRunTapesHandler(t *testing.T) {
	fake := &fakeTemporalClient{historyFunc: func(string) client.HistoryEventIterator {
		return &fakeHistoryIterator{events: buildSuccessfulRunHistory(t)}
	}}
	handler := newMux(newHandler(fake, envLookup(nil)))

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
			historyFunc: func(string) client.HistoryEventIterator {
				// Every call gets the same iterator in this fake shape, so
				// dispatch on call count instead: the first call answers
				// run-good, the second run-gone. Order matches Executions
				// above only if listTapes calls sequentially per execution
				// with no reordering before dispatch, which the SetLimit(8)
				// errgroup does not guarantee — so key on content instead.
				return nil
			},
		}

		// Route per runID explicitly: fakeTemporalClient.GetWorkflowHistory
		// (history_test.go) ignores the runID argument by construction (see
		// its doc comment), so give listTapes-specific behavior via a
		// dedicated historyFunc keyed by a call counter guarded by identity
		// instead of runID text, using a small closure over both fixtures.
		calls := 0
		fixtures := [][]*historypb.HistoryEvent{buildSuccessfulRunHistory(t), nil}
		errs := []error{nil, serviceerror.NewNotFound("workflow execution not found")}

		fake.historyFunc = func(string) client.HistoryEventIterator {
			i := calls
			calls++

			if i >= len(fixtures) {
				i = len(fixtures) - 1
			}

			return &fakeHistoryIterator{events: fixtures[i], err: errs[i]}
		}

		handler := newMux(newHandler(fake, envLookup(nil)))

		recorder := doJSON(t, handler, http.MethodGet, "/api/tapes", nil)
		require.Equal(t, http.StatusOK, recorder.Code)

		var body AggregateTapesResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

		assert.Len(t, body.Tapes, 2, "the reconstructable run's two tapes must still be listed")
		assert.Len(t, body.RunErrors, 1, "the unreconstructable run must degrade, not fail the whole listing")
	})
}

// envLookup returns a getenv func backed by m, for tests that construct a
// handler directly (mirroring runsapi_test.go's own pattern where present).
func envLookup(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}
