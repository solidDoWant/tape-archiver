package runsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/runsubmit"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

type fakeEncodedValue struct {
	value interface{}
	err   error
}

func (f fakeEncodedValue) HasValue() bool { return f.value != nil }

func (f fakeEncodedValue) Get(valuePtr interface{}) error {
	if f.err != nil {
		return f.err
	}

	data, err := json.Marshal(f.value)
	if err != nil {
		return err
	}

	return json.Unmarshal(data, valuePtr)
}

// fakeTemporalClient fakes the Temporal dependency, not runsapi itself.
type fakeTemporalClient struct {
	listResponse *workflowservice.ListWorkflowExecutionsResponse
	listErr      error

	describeResponse *workflowservice.DescribeWorkflowExecutionResponse
	describeErr      error

	// queryResult/queryErr answer backup.LastCompletedPhaseQuery.
	queryResult interface{}
	queryErr    error

	// currentPauseResult/currentPauseErr answer backup.CurrentPauseQuery.
	currentPauseResult interface{}
	currentPauseErr    error

	executeRun      client.WorkflowRun
	executeErr      error
	executeOptions  client.StartWorkflowOptions
	executeCaptured bool
	// executeConfig is the config that actually reached ExecuteWorkflow, after overrides.
	executeConfig *config.Config

	signalErr        error
	signalCaptured   bool
	signalWorkflowID string
	signalRunID      string
	signalName       string

	cancelErr        error
	cancelCaptured   bool
	cancelWorkflowID string
	cancelRunID      string

	// historyFunc answers GetWorkflowHistory; nil yields an empty history.
	historyFunc func(runID string) client.HistoryEventIterator

	taskQueueResponse *workflowservice.DescribeTaskQueueResponse
	taskQueueErr      error
	taskQueueCalls    []string
}

func (f *fakeTemporalClient) DescribeTaskQueue(_ context.Context, taskQueue string, _ enumspb.TaskQueueType) (*workflowservice.DescribeTaskQueueResponse, error) {
	f.taskQueueCalls = append(f.taskQueueCalls, taskQueue)

	return f.taskQueueResponse, f.taskQueueErr
}

func (f *fakeTemporalClient) ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	return f.listResponse, f.listErr
}

func (f *fakeTemporalClient) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return f.describeResponse, f.describeErr
}

func (f *fakeTemporalClient) QueryWorkflow(_ context.Context, _ string, _ string, queryType string, _ ...interface{}) (converter.EncodedValue, error) {
	if queryType == backup.CurrentPauseQuery {
		if f.currentPauseErr != nil {
			return nil, f.currentPauseErr
		}

		return fakeEncodedValue{value: f.currentPauseResult}, nil
	}

	if f.queryErr != nil {
		return nil, f.queryErr
	}

	return fakeEncodedValue{value: f.queryResult}, nil
}

func (f *fakeTemporalClient) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, _ interface{}, args ...interface{}) (client.WorkflowRun, error) {
	f.executeOptions = options
	f.executeCaptured = true

	if len(args) == 1 {
		if cfg, ok := args[0].(*config.Config); ok {
			f.executeConfig = cfg
		}
	}

	return f.executeRun, f.executeErr
}

func (f *fakeTemporalClient) SignalWorkflow(_ context.Context, workflowID, runID, signalName string, _ interface{}) error {
	f.signalCaptured = true
	f.signalWorkflowID = workflowID
	f.signalRunID = runID
	f.signalName = signalName

	return f.signalErr
}

func (f *fakeTemporalClient) CancelWorkflow(_ context.Context, workflowID, runID string) error {
	f.cancelCaptured = true
	f.cancelWorkflowID = workflowID
	f.cancelRunID = runID

	return f.cancelErr
}

type fakeWorkflowRun struct {
	workflowID string
	runID      string
}

func (f fakeWorkflowRun) GetID() string    { return f.workflowID }
func (f fakeWorkflowRun) GetRunID() string { return f.runID }

// GetFirstExecutionRunID: a fake run is its own single-execution chain.
func (f fakeWorkflowRun) GetFirstExecutionRunID() string { return f.runID }
func (f fakeWorkflowRun) Get(context.Context, interface{}) error {
	return nil
}
func (f fakeWorkflowRun) GetWithOptions(context.Context, interface{}, client.WorkflowRunGetOptions) error {
	return nil
}

func executionInfo(runID string, status enumspb.WorkflowExecutionStatus, start time.Time, close *time.Time) *workflowpb.WorkflowExecutionInfo {
	info := &workflowpb.WorkflowExecutionInfo{
		Execution: &commonpb.WorkflowExecution{WorkflowId: backup.WorkflowID, RunId: runID},
		Status:    status,
		StartTime: timestamppb.New(start),
		TaskQueue: backup.TaskQueue,
	}

	if close != nil {
		info.CloseTime = timestamppb.New(*close)
	}

	return info
}

func TestToRunSummaryDryRun(t *testing.T) {
	start := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	dryRunMemo := func(t *testing.T, dryRun bool) *commonpb.Memo {
		t.Helper()

		payload, err := converter.GetDefaultDataConverter().ToPayload(dryRun)
		require.NoError(t, err)

		return &commonpb.Memo{Fields: map[string]*commonpb.Payload{runsubmit.MemoKeyDryRun: payload}}
	}

	tests := []struct {
		name string
		memo *commonpb.Memo
		want bool
	}{
		{name: "a true dry-run memo decodes to a dry-run", memo: dryRunMemo(t, true), want: true},
		{name: "a false dry-run memo decodes to production", memo: dryRunMemo(t, false), want: false},
		{name: "a run with no memo reads as production", memo: nil, want: false},
		{name: "an empty memo reads as production", memo: &commonpb.Memo{}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, nil)
			info.Memo = test.memo

			assert.Equal(t, test.want, toRunSummary(info).DryRun)
		})
	}
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, path, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if body != nil {
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), body))
	}

	return recorder
}

func TestListRuns(t *testing.T) {
	start := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	closeTime := start.Add(time.Hour)

	tests := []struct {
		name         string
		client       *fakeTemporalClient
		wantStatus   int
		wantRunCount int
		errAssert    require.ErrorAssertionFunc
	}{
		{
			name: "empty visibility returns an empty list, not an error",
			client: &fakeTemporalClient{
				listResponse: &workflowservice.ListWorkflowExecutionsResponse{},
			},
			wantStatus:   http.StatusOK,
			wantRunCount: 0,
		},
		{
			name: "executions are mapped into run summaries",
			client: &fakeTemporalClient{
				listResponse: &workflowservice.ListWorkflowExecutionsResponse{
					Executions: []*workflowpb.WorkflowExecutionInfo{
						executionInfo("run-2", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, &closeTime),
						executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, start, nil),
					},
				},
			},
			wantStatus:   http.StatusOK,
			wantRunCount: 2,
		},
		{
			name: "a Temporal error is reported as a 502, not a 500 or hang",
			client: &fakeTemporalClient{
				listErr: assertError{"visibility unavailable"},
			},
			wantStatus: http.StatusBadGateway,
			errAssert:  require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errAssert := test.errAssert
			if errAssert == nil {
				errAssert = require.NoError
			}

			handler := New(test.client)

			var body RunsResponse

			recorder := doJSON(t, handler, http.MethodGet, "/api/runs", nil)
			assert.Equal(t, test.wantStatus, recorder.Code)

			if test.wantStatus == http.StatusOK {
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
				assert.Len(t, body.Runs, test.wantRunCount)
				errAssert(t, nil)

				return
			}

			errAssert(t, decodeAPIError(t, recorder))
		})
	}

	t.Run("the visibility query filters to the singleton workflow ID", func(t *testing.T) {
		client := &fakeTemporalClient{listResponse: &workflowservice.ListWorkflowExecutionsResponse{}}

		var captured *workflowservice.ListWorkflowExecutionsRequest

		spy := spyClient{TemporalClient: client, onList: func(request *workflowservice.ListWorkflowExecutionsRequest) {
			captured = request
		}}
		handler := New(spy)

		doJSON(t, handler, http.MethodGet, "/api/runs", nil)

		require.NotNil(t, captured)
		assert.Contains(t, captured.GetQuery(), backup.WorkflowID)
		// SQL visibility rejects ORDER BY.
		assert.NotContains(t, captured.GetQuery(), "ORDER BY")
	})

	t.Run("runs are sorted newest-first regardless of the order Temporal returns them in", func(t *testing.T) {
		older := start
		newer := start.Add(time.Hour)

		client := &fakeTemporalClient{
			listResponse: &workflowservice.ListWorkflowExecutionsResponse{
				Executions: []*workflowpb.WorkflowExecutionInfo{
					executionInfo("run-older", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, older, &closeTime),
					executionInfo("run-newer", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, newer, nil),
				},
			},
		}
		handler := New(client)

		var body RunsResponse

		recorder := doJSON(t, handler, http.MethodGet, "/api/runs", &body)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Len(t, body.Runs, 2)
		assert.Equal(t, "run-newer", body.Runs[0].RunID)
		assert.Equal(t, "run-older", body.Runs[1].RunID)
	})

	t.Run("run fields round-trip through the response", func(t *testing.T) {
		client := &fakeTemporalClient{
			listResponse: &workflowservice.ListWorkflowExecutionsResponse{
				Executions: []*workflowpb.WorkflowExecutionInfo{
					executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, &closeTime),
				},
			},
		}
		handler := New(client)

		var body RunsResponse

		recorder := doJSON(t, handler, http.MethodGet, "/api/runs", &body)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Len(t, body.Runs, 1)

		run := body.Runs[0]
		assert.Equal(t, backup.WorkflowID, run.WorkflowID)
		assert.Equal(t, "run-1", run.RunID)
		assert.Equal(t, enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED.String(), run.Status)
		assert.True(t, start.Equal(run.StartTime))
		require.NotNil(t, run.CloseTime)
		assert.True(t, closeTime.Equal(*run.CloseTime))
	})
}

type spyClient struct {
	TemporalClient
	onList func(*workflowservice.ListWorkflowExecutionsRequest)
}

func (s spyClient) ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	s.onList(request)

	return s.TemporalClient.ListWorkflow(ctx, request)
}

func TestGetRun(t *testing.T) {
	start := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name             string
		runID            string
		client           *fakeTemporalClient
		wantStatus       int
		wantPhase        string
		wantPhaseUnknown bool
		wantPauseKind    string
		wantPauseUnknown bool
		errAssert        require.ErrorAssertionFunc
	}{
		{
			name:  "a known run returns detail including the last completed phase",
			runID: "run-1",
			client: &fakeTemporalClient{
				describeResponse: &workflowservice.DescribeWorkflowExecutionResponse{
					WorkflowExecutionInfo: executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, start, nil),
				},
				queryResult: "Verify",
			},
			wantStatus: http.StatusOK,
			wantPhase:  "Verify",
		},
		{
			name:  "a paused run's detail includes the current pause state",
			runID: "run-1",
			client: &fakeTemporalClient{
				describeResponse: &workflowservice.DescribeWorkflowExecutionResponse{
					WorkflowExecutionInfo: executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, start, nil),
				},
				queryResult:        "Write",
				currentPauseResult: backup.CurrentPause{Kind: backup.PauseWriteFailure, Phase: backup.PhaseWrite, AffectedTapes: []string{"TA0001L6"}},
			},
			wantStatus:    http.StatusOK,
			wantPhase:     "Write",
			wantPauseKind: "write-failure",
		},
		{
			// Unknown only when the history fallback (issue #329) also fails.
			name:  "a failed current-pause query with unreadable history is reported unknown, not none",
			runID: "run-1",
			client: &fakeTemporalClient{
				describeResponse: &workflowservice.DescribeWorkflowExecutionResponse{
					WorkflowExecutionInfo: executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, start, nil),
				},
				queryResult:     "Write",
				currentPauseErr: assertError{"no worker polling"},
				historyFunc: func(string) client.HistoryEventIterator {
					return &fakeHistoryIterator{err: assertError{"history unavailable"}}
				},
			},
			wantStatus:       http.StatusOK,
			wantPhase:        "Write",
			wantPauseKind:    "",
			wantPauseUnknown: true,
		},
		{
			// Issue #329: no worker polling (KEDA at zero) but the run is paused.
			name:  "a failed current-pause query falls back to the history-derived pause",
			runID: "run-1",
			client: &fakeTemporalClient{
				describeResponse: &workflowservice.DescribeWorkflowExecutionResponse{
					WorkflowExecutionInfo: executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, start, nil),
				},
				queryResult:     "Load",
				currentPauseErr: assertError{"no worker polling"},
				historyFunc: func(string) client.HistoryEventIterator {
					return &fakeHistoryIterator{events: pausedRunHistory(t, notifyWritePathPauseActivity, backup.WritePathPauseInput{
						Phase: backup.PhaseWrite, AffectedTapes: []string{"TA0001L6"}, ReloadSlots: []int{3}, ErrorSummary: "drive 0 write failed",
					})}
				},
			},
			wantStatus:    http.StatusOK,
			wantPhase:     "Load",
			wantPauseKind: "write-failure",
		},
		{
			name:  "an unknown run returns 404, not a 500 or hang",
			runID: "does-not-exist",
			client: &fakeTemporalClient{
				describeErr: &serviceerror.NotFound{Message: "not found"},
			},
			wantStatus: http.StatusNotFound,
			errAssert:  require.Error,
		},
		{
			name:  "a non-NotFound Temporal error is reported as a 502",
			runID: "run-1",
			client: &fakeTemporalClient{
				describeErr: assertError{"transient RPC failure"},
			},
			wantStatus: http.StatusBadGateway,
			errAssert:  require.Error,
		},
		{
			name:  "a malformed run ID is reported as a 400, not a 404 or 502",
			runID: "not-a-uuid",
			client: &fakeTemporalClient{
				describeErr: &serviceerror.InvalidArgument{Message: "Invalid RunId."},
			},
			wantStatus: http.StatusBadRequest,
			errAssert:  require.Error,
		},
		{
			// Issue #323: a read failure must not look like "nothing completed".
			name:  "a failed phase query with unreadable history reports empty and unknown",
			runID: "run-1",
			client: &fakeTemporalClient{
				describeResponse: &workflowservice.DescribeWorkflowExecutionResponse{
					WorkflowExecutionInfo: executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, start, nil),
				},
				queryErr: assertError{"no worker polling"},
				historyFunc: func(string) client.HistoryEventIterator {
					return &fakeHistoryIterator{err: assertError{"history unavailable"}}
				},
			},
			wantStatus:       http.StatusOK,
			wantPhase:        "",
			wantPhaseUnknown: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errAssert := test.errAssert
			if errAssert == nil {
				errAssert = require.NoError
			}

			handler := New(test.client)

			recorder := doJSON(t, handler, http.MethodGet, "/api/runs/"+test.runID, nil)
			assert.Equal(t, test.wantStatus, recorder.Code)

			if test.wantStatus == http.StatusOK {
				var detail RunDetail
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &detail))
				assert.Equal(t, test.runID, detail.RunID)
				assert.Equal(t, test.wantPhase, detail.LastCompletedPhase)
				assert.Equal(t, test.wantPhaseUnknown, detail.LastCompletedPhaseUnknown)
				assert.Equal(t, test.wantPauseKind, detail.CurrentPause.Kind)
				assert.Equal(t, test.wantPauseUnknown, detail.CurrentPause.Unknown)
				errAssert(t, nil)

				return
			}

			errAssert(t, decodeAPIError(t, recorder))
		})
	}
}

func TestResumeRun(t *testing.T) {
	tests := []struct {
		name       string
		client     *fakeTemporalClient
		wantStatus int
		// wantSignalAttempted is whether SignalWorkflow was called, even if it failed.
		wantSignalAttempted bool
		errAssert           require.ErrorAssertionFunc
	}{
		{
			name:                "a write-failure pause resumes: 202 and the resume signal is sent",
			client:              &fakeTemporalClient{currentPauseResult: backup.CurrentPause{Kind: backup.PauseWriteFailure}},
			wantStatus:          http.StatusAccepted,
			wantSignalAttempted: true,
		},
		{
			name:                "an eject pause resumes too",
			client:              &fakeTemporalClient{currentPauseResult: backup.CurrentPause{Kind: backup.PauseEject}},
			wantStatus:          http.StatusAccepted,
			wantSignalAttempted: true,
		},
		{
			name:                "a burn pause resumes too",
			client:              &fakeTemporalClient{currentPauseResult: backup.CurrentPause{Kind: backup.PauseBurn}},
			wantStatus:          http.StatusAccepted,
			wantSignalAttempted: true,
		},
		{
			name:       "a run that is not paused rejects resume with 409, not a signal sent into the void",
			client:     &fakeTemporalClient{currentPauseResult: backup.CurrentPause{Kind: backup.PauseNone}},
			wantStatus: http.StatusConflict,
			errAssert:  require.Error,
		},
		{
			// Issue #329: no worker polling must not block resume.
			name: "a failed pause query falls back to history and resumes anyway",
			client: &fakeTemporalClient{
				currentPauseErr: assertError{"no worker polling"},
				historyFunc: func(t *testing.T) func(string) client.HistoryEventIterator {
					return func(string) client.HistoryEventIterator {
						return &fakeHistoryIterator{events: pausedRunHistory(t, notifyWritePathPauseActivity, backup.WritePathPauseInput{Phase: backup.PhaseWrite})}
					}
				}(t),
			},
			wantStatus:          http.StatusAccepted,
			wantSignalAttempted: true,
		},
		{
			// The live-query NotFound is preserved for classification.
			name: "an unknown run returns 404, not a 500 or hang",
			client: &fakeTemporalClient{
				currentPauseErr: &serviceerror.NotFound{Message: "not found"},
				historyFunc: func(string) client.HistoryEventIterator {
					return &fakeHistoryIterator{err: &serviceerror.NotFound{Message: "not found"}}
				},
			},
			wantStatus: http.StatusNotFound,
			errAssert:  require.Error,
		},
		{
			name: "a non-NotFound pause-query failure with unreadable history is a 502",
			client: &fakeTemporalClient{
				currentPauseErr: assertError{"transient RPC failure"},
				historyFunc: func(string) client.HistoryEventIterator {
					return &fakeHistoryIterator{err: assertError{"transient RPC failure"}}
				},
			},
			wantStatus: http.StatusBadGateway,
			errAssert:  require.Error,
		},
		{
			name: "a SignalWorkflow failure is reported, not swallowed",
			client: &fakeTemporalClient{
				currentPauseResult: backup.CurrentPause{Kind: backup.PauseWriteFailure},
				signalErr:          assertError{"signal RPC failed"},
			},
			wantStatus:          http.StatusBadGateway,
			wantSignalAttempted: true,
			errAssert:           require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errAssert := test.errAssert
			if errAssert == nil {
				errAssert = require.NoError
			}

			handler := New(test.client)

			recorder := postJSON(t, handler, "/api/runs/run-1/resume", nil, nil)
			assert.Equal(t, test.wantStatus, recorder.Code)
			assert.Equal(t, test.wantSignalAttempted, test.client.signalCaptured)

			if test.wantStatus == http.StatusAccepted {
				assert.Equal(t, backup.WorkflowID, test.client.signalWorkflowID)
				assert.Equal(t, "run-1", test.client.signalRunID)
				assert.Equal(t, backup.OperatorResumeSignal, test.client.signalName)
				errAssert(t, nil)

				return
			}

			errAssert(t, decodeAPIError(t, recorder))
		})
	}
}

func TestAbortRun(t *testing.T) {
	tests := []struct {
		name       string
		client     *fakeTemporalClient
		wantStatus int
		// wantSignalAttempted is whether SignalWorkflow was called, even if it failed.
		wantSignalAttempted bool
		errAssert           require.ErrorAssertionFunc
	}{
		{
			name:                "a write-failure pause aborts: 202 and the abort signal is sent",
			client:              &fakeTemporalClient{currentPauseResult: backup.CurrentPause{Kind: backup.PauseWriteFailure}},
			wantStatus:          http.StatusAccepted,
			wantSignalAttempted: true,
		},
		{
			name:                "a burn pause aborts too",
			client:              &fakeTemporalClient{currentPauseResult: backup.CurrentPause{Kind: backup.PauseBurn}},
			wantStatus:          http.StatusAccepted,
			wantSignalAttempted: true,
		},
		{
			name:       "an eject pause rejects abort with 409; the signal is never sent",
			client:     &fakeTemporalClient{currentPauseResult: backup.CurrentPause{Kind: backup.PauseEject}},
			wantStatus: http.StatusConflict,
			errAssert:  require.Error,
		},
		{
			name:       "a run that is not paused rejects abort with 409",
			client:     &fakeTemporalClient{currentPauseResult: backup.CurrentPause{Kind: backup.PauseNone}},
			wantStatus: http.StatusConflict,
			errAssert:  require.Error,
		},
		{
			// Issue #329: no worker polling must not block abort.
			name: "a failed pause query falls back to history and aborts anyway",
			client: &fakeTemporalClient{
				currentPauseErr: assertError{"no worker polling"},
				historyFunc: func(t *testing.T) func(string) client.HistoryEventIterator {
					return func(string) client.HistoryEventIterator {
						return &fakeHistoryIterator{events: pausedRunHistory(t, notifyBurnPauseActivity, backup.BurnPauseInput{Devices: []string{"/dev/sr0"}})}
					}
				}(t),
			},
			wantStatus:          http.StatusAccepted,
			wantSignalAttempted: true,
		},
		{
			name: "an unknown run returns 404, not a 500 or hang",
			client: &fakeTemporalClient{
				currentPauseErr: &serviceerror.NotFound{Message: "not found"},
				historyFunc: func(string) client.HistoryEventIterator {
					return &fakeHistoryIterator{err: &serviceerror.NotFound{Message: "not found"}}
				},
			},
			wantStatus: http.StatusNotFound,
			errAssert:  require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errAssert := test.errAssert
			if errAssert == nil {
				errAssert = require.NoError
			}

			handler := New(test.client)

			recorder := postJSON(t, handler, "/api/runs/run-1/abort", nil, nil)
			assert.Equal(t, test.wantStatus, recorder.Code)
			assert.Equal(t, test.wantSignalAttempted, test.client.signalCaptured)

			if test.wantStatus == http.StatusAccepted {
				assert.Equal(t, backup.WorkflowID, test.client.signalWorkflowID)
				assert.Equal(t, "run-1", test.client.signalRunID)
				assert.Equal(t, backup.OperatorAbortSignal, test.client.signalName)
				errAssert(t, nil)

				return
			}

			errAssert(t, decodeAPIError(t, recorder))
		})
	}
}

func TestCancelRun(t *testing.T) {
	start := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	closeTime := start.Add(time.Hour)

	running := &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, start, nil),
	}
	completed := &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, start, &closeTime),
	}

	tests := []struct {
		name       string
		client     *fakeTemporalClient
		wantStatus int
		// wantCancelAttempted is whether CancelWorkflow was called, even if it failed.
		wantCancelAttempted bool
		errAssert           require.ErrorAssertionFunc
	}{
		{
			name:                "a running run cancels: 202 and CancelWorkflow is called",
			client:              &fakeTemporalClient{describeResponse: running},
			wantStatus:          http.StatusAccepted,
			wantCancelAttempted: true,
		},
		{
			name:       "an already-closed run rejects cancel with 409; CancelWorkflow is never called",
			client:     &fakeTemporalClient{describeResponse: completed},
			wantStatus: http.StatusConflict,
			errAssert:  require.Error,
		},
		{
			name:       "an unknown run returns 404, not a 500 or hang",
			client:     &fakeTemporalClient{describeErr: &serviceerror.NotFound{Message: "not found"}},
			wantStatus: http.StatusNotFound,
			errAssert:  require.Error,
		},
		{
			name:       "a non-NotFound describe failure is a 502",
			client:     &fakeTemporalClient{describeErr: assertError{"transient RPC failure"}},
			wantStatus: http.StatusBadGateway,
			errAssert:  require.Error,
		},
		{
			name: "a CancelWorkflow failure is reported, not swallowed",
			client: &fakeTemporalClient{
				describeResponse: running,
				cancelErr:        assertError{"cancel RPC failed"},
			},
			wantStatus:          http.StatusBadGateway,
			wantCancelAttempted: true,
			errAssert:           require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errAssert := test.errAssert
			if errAssert == nil {
				errAssert = require.NoError
			}

			handler := New(test.client)

			recorder := postJSON(t, handler, "/api/runs/run-1/cancel", nil, nil)
			assert.Equal(t, test.wantStatus, recorder.Code)
			assert.Equal(t, test.wantCancelAttempted, test.client.cancelCaptured)

			if test.wantStatus == http.StatusAccepted {
				assert.Equal(t, backup.WorkflowID, test.client.cancelWorkflowID)
				assert.Equal(t, "run-1", test.client.cancelRunID)
				errAssert(t, nil)

				return
			}

			errAssert(t, decodeAPIError(t, recorder))
		})
	}
}

func TestWriteErrorSanitizesUpstreamFaults(t *testing.T) {
	t.Run("a 502 does not leak the raw upstream error text", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		writeError(recorder, http.StatusBadGateway, errors.New("dial tcp 10.0.0.5:7233: connection refused"))

		require.Equal(t, http.StatusBadGateway, recorder.Code)
		body := decodeAPIError(t, recorder)
		assert.NotContains(t, body.Error(), "10.0.0.5")
		assert.NotContains(t, body.Error(), "connection refused")
	})

	t.Run("a 504 does not leak the raw upstream error text", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		writeError(recorder, http.StatusGatewayTimeout, errors.New("context deadline exceeded: temporal-frontend.internal:7233"))

		require.Equal(t, http.StatusGatewayTimeout, recorder.Code)
		assert.NotContains(t, decodeAPIError(t, recorder).Error(), "temporal-frontend.internal")
	})

	t.Run("a 4xx keeps its actionable message", func(t *testing.T) {
		recorder := httptest.NewRecorder()

		writeError(recorder, http.StatusBadRequest, errors.New("runID is required"))

		assert.Contains(t, decodeAPIError(t, recorder).Error(), "runID is required")
	})
}

// decodeAPIError turns a JSON error body into an error for require.ErrorAssertionFunc.
func decodeAPIError(t *testing.T, recorder *httptest.ResponseRecorder) error {
	t.Helper()

	var body errorResponse

	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.NotEmpty(t, body.Error)

	return errors.New(body.Error)
}

type assertError struct{ msg string }

func (e assertError) Error() string { return e.msg }

const validSubmitConfigJSON = `{
  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
  "copies": 2,
  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
  "redundancy": {"targetPercentage": 10},
  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"}
}`

func postJSON(t *testing.T, handler http.Handler, path string, body []byte, response interface{}) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if response != nil {
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), response))
	}

	return recorder
}

func TestListAllBackupExecutionsBoundsEmptyTokenedPages(t *testing.T) {
	// The fake returns the same tokened empty page forever; only the page cap ends it.
	client := &fakeTemporalClient{listResponse: &workflowservice.ListWorkflowExecutionsResponse{
		NextPageToken: []byte("more"),
	}}

	type result struct {
		all []*workflowpb.WorkflowExecutionInfo
		err error
	}

	done := make(chan result, 1)

	go func() {
		all, err := listAllBackupExecutions(t.Context(), client)
		done <- result{all, err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Empty(t, got.all)
	case <-time.After(5 * time.Second):
		t.Fatal("listAllBackupExecutions did not terminate — the page cap is not bounding empty tokened pages")
	}
}

func TestRunExistsInVisibilityBoundsEmptyTokenedPages(t *testing.T) {
	// writeHistoryError's 404-vs-410 path must be bounded by the page cap too.
	client := &fakeTemporalClient{listResponse: &workflowservice.ListWorkflowExecutionsResponse{
		NextPageToken: []byte("more"),
	}}

	type result struct {
		found bool
		err   error
	}

	done := make(chan result, 1)

	go func() {
		found, err := runExistsInVisibility(t.Context(), client, "some-run-id")
		done <- result{found, err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.False(t, got.found)
	case <-time.After(5 * time.Second):
		t.Fatal("runExistsInVisibility did not terminate — the page cap is not bounding empty tokened pages")
	}
}

func TestSubmitRun(t *testing.T) {
	mhvtlEnv := func(name string) string {
		switch name {
		case "MHVTL_CHANGER_DEV":
			return "/dev/sch9"
		case "MHVTL_DRIVE0_DEV":
			return "/dev/nst8"
		case "MHVTL_DRIVE1_DEV":
			return "/dev/nst9"
		default:
			return ""
		}
	}

	ownedDevices := []Option{WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, "")}

	tests := []struct {
		name       string
		body       []byte
		getenv     func(string) string
		opts       []Option
		client     *fakeTemporalClient
		wantStatus int
		errAssert  require.ErrorAssertionFunc
	}{
		{
			name:       "a valid config submits and returns the run ID",
			body:       []byte(`{"config": ` + validSubmitConfigJSON + `}`),
			getenv:     func(string) string { return "" },
			opts:       ownedDevices,
			client:     &fakeTemporalClient{executeRun: fakeWorkflowRun{workflowID: backup.WorkflowID, runID: "run-xyz"}},
			wantStatus: http.StatusCreated,
		},
		{
			// CLAUDE.md Hardware and Safety: no client-supplied real devices.
			name:       "a production run without deploy-owned devices is rejected",
			body:       []byte(`{"config": ` + validSubmitConfigJSON + `}`),
			getenv:     func(string) string { return "" },
			client:     &fakeTemporalClient{},
			wantStatus: http.StatusBadRequest,
			errAssert:  require.Error,
		},
		{
			name:       "dry-run with mhvtl env set redirects devices and submits",
			body:       []byte(`{"config": ` + validSubmitConfigJSON + `, "dryRun": true}`),
			getenv:     mhvtlEnv,
			client:     &fakeTemporalClient{executeRun: fakeWorkflowRun{workflowID: backup.WorkflowID, runID: "run-dry"}},
			wantStatus: http.StatusCreated,
		},
		{
			// AC: dry-run fails closed rather than falling back to real devices.
			name:       "dry-run without mhvtl env fails closed and submits nothing",
			body:       []byte(`{"config": ` + validSubmitConfigJSON + `, "dryRun": true}`),
			getenv:     func(string) string { return "" },
			client:     &fakeTemporalClient{},
			wantStatus: http.StatusBadRequest,
			errAssert:  require.Error,
		},
		{
			name:       "an invalid config is rejected before any Temporal contact",
			body:       []byte(`{"config": {"copies": 2}}`),
			getenv:     func(string) string { return "" },
			client:     &fakeTemporalClient{},
			wantStatus: http.StatusBadRequest,
			errAssert:  require.Error,
		},
		{
			name:       "an empty config is rejected",
			body:       []byte(`{}`),
			getenv:     func(string) string { return "" },
			client:     &fakeTemporalClient{},
			wantStatus: http.StatusBadRequest,
			errAssert:  require.Error,
		},
		{
			name:       "malformed JSON is rejected, not a 500",
			body:       []byte(`not json`),
			getenv:     func(string) string { return "" },
			client:     &fakeTemporalClient{},
			wantStatus: http.StatusBadRequest,
			errAssert:  require.Error,
		},
		{
			// Must not silently decode only the first value.
			name:       "trailing content after the JSON object is rejected",
			body:       []byte(`{"config": ` + validSubmitConfigJSON + `} {"config": ` + validSubmitConfigJSON + `}`),
			getenv:     func(string) string { return "" },
			client:     &fakeTemporalClient{},
			wantStatus: http.StatusBadRequest,
			errAssert:  require.Error,
		},
		{
			// AC: a misspelled dryRun must not silently submit a real run.
			name:       "an unrecognized envelope field is rejected, not silently ignored",
			body:       []byte(`{"config": ` + validSubmitConfigJSON + `, "isDryRun": true}`),
			getenv:     func(string) string { return "" },
			client:     &fakeTemporalClient{},
			wantStatus: http.StatusBadRequest,
			errAssert:  require.Error,
		},
		{
			// Padding is inside "config" to isolate the size cap from unknown-field rejection.
			name:       "a request body over the size cap is rejected as 413, not a generic 400",
			body:       []byte(`{"config": "` + strings.Repeat("x", maxSubmitBodyBytes) + `"}`),
			getenv:     func(string) string { return "" },
			client:     &fakeTemporalClient{},
			wantStatus: http.StatusRequestEntityTooLarge,
			errAssert:  require.Error,
		},
		{
			name:   "a run already in progress is a 409 conflict, not a 500 or silent replace",
			body:   []byte(`{"config": ` + validSubmitConfigJSON + `}`),
			getenv: func(string) string { return "" },
			opts:   ownedDevices,
			client: &fakeTemporalClient{
				executeErr: serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "req-1", "run-existing"),
			},
			wantStatus: http.StatusConflict,
			errAssert:  require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errAssert := test.errAssert
			if errAssert == nil {
				errAssert = require.NoError
			}

			handler := newMux(newHandler(test.client, test.getenv, test.opts...))

			var response SubmitRunResponse

			recorder := postJSON(t, handler, "/api/runs", test.body, nil)
			assert.Equal(t, test.wantStatus, recorder.Code)

			if test.wantStatus == http.StatusCreated {
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
				assert.NotEmpty(t, response.RunID)
				assert.Equal(t, backup.WorkflowID, response.WorkflowID)
				assert.Equal(t, "/api/runs/"+response.RunID, recorder.Header().Get("Location"))
				require.True(t, test.client.executeCaptured)
				errAssert(t, nil)

				return
			}

			// Only the 409 case reaches Temporal; all other rejections precede it.
			wantExecuted := test.wantStatus == http.StatusConflict
			assert.Equal(t, wantExecuted, test.client.executeCaptured)

			errAssert(t, decodeAPIError(t, recorder))
		})
	}

	// Issue #304: deploy-owned devices/webhook override whatever a client submits.
	t.Run("deploy config overrides the submitted library devices and webhook", func(t *testing.T) {
		// Devices/webhook deliberately differ from the deployment's.
		rogue := []byte(`{"config": {
		  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
		  "copies": 2,
		  "library": {"changer": "/dev/rogue-sch", "drives": ["/dev/rogue0"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
		  "redundancy": {"targetPercentage": 10},
		  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
		  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/rogue/rogue"}
		}}`)

		fake := &fakeTemporalClient{executeRun: fakeWorkflowRun{workflowID: backup.WorkflowID, runID: "run-deploy"}}
		handler := newMux(newHandler(fake, func(string) string { return "" },
			WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, "https://discord.com/api/webhooks/deploy/deploy")))

		recorder := postJSON(t, handler, "/api/runs", rogue, nil)

		require.Equal(t, http.StatusCreated, recorder.Code)
		require.True(t, fake.executeCaptured)
		require.NotNil(t, fake.executeConfig)
		assert.Equal(t, "/dev/sch0", fake.executeConfig.Library.Changer)
		assert.Equal(t, []string{"/dev/nst0", "/dev/nst1"}, fake.executeConfig.Library.Drives)
		assert.Equal(t, "https://discord.com/api/webhooks/deploy/deploy", fake.executeConfig.Delivery.WebhookURL)
	})

	// A dry run must never touch real hardware (CLAUDE.md Hardware and Safety).
	t.Run("a dry-run's mhvtl override wins over deploy config devices", func(t *testing.T) {
		fake := &fakeTemporalClient{executeRun: fakeWorkflowRun{workflowID: backup.WorkflowID, runID: "run-dry-deploy"}}
		handler := newMux(newHandler(fake, mhvtlEnv,
			WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, "")))

		recorder := postJSON(t, handler, "/api/runs", []byte(`{"config": `+validSubmitConfigJSON+`, "dryRun": true}`), nil)

		require.Equal(t, http.StatusCreated, recorder.Code)
		require.NotNil(t, fake.executeConfig)
		assert.Equal(t, "/dev/sch9", fake.executeConfig.Library.Changer)
		assert.Equal(t, []string{"/dev/nst8", "/dev/nst9"}, fake.executeConfig.Library.Drives)
	})

	// Issue #382: only the dry-run override may choose a mirror source.
	t.Run("a client-supplied mirror source never reaches the workflow", func(t *testing.T) {
		rogueMirror := strings.Replace(validSubmitConfigJSON, `"changer": "/dev/sch0",`,
			`"changer": "/dev/sch0", "mirrorSourceChanger": "/dev/rogue-sch",`, 1)

		tests := []struct {
			name   string
			dryRun bool
			want   string
		}{
			{name: "production run", want: ""},
			{name: "dry run", dryRun: true, want: "/dev/sch0"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				fake := &fakeTemporalClient{executeRun: fakeWorkflowRun{workflowID: backup.WorkflowID, runID: "run-mirror"}}
				handler := newMux(newHandler(fake, mhvtlEnv, ownedDevices...))

				body := fmt.Sprintf(`{"config": %s, "dryRun": %t}`, rogueMirror, test.dryRun)
				recorder := postJSON(t, handler, "/api/runs", []byte(body), nil)

				require.Equal(t, http.StatusCreated, recorder.Code)
				require.NotNil(t, fake.executeConfig)
				assert.Equal(t, test.want, fake.executeConfig.Library.MirrorSourceChanger)
			})
		}
	})

	t.Run("a production run with no deploy-owned devices is rejected before Temporal", func(t *testing.T) {
		fake := &fakeTemporalClient{}
		handler := newMux(newHandler(fake, func(string) string { return "" }))

		recorder := postJSON(t, handler, "/api/runs", []byte(`{"config": `+validSubmitConfigJSON+`}`), nil)

		assert.Equal(t, http.StatusBadRequest, recorder.Code)
		assert.False(t, fake.executeCaptured)
	})

	// The report embeds the escrow private key, so it must not go to a client webhook.
	t.Run("a client-supplied webhook on a production run without a deploy webhook is rejected", func(t *testing.T) {
		configWithWebhook := `{
		  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
		  "copies": 2,
		  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
		  "redundancy": {"targetPercentage": 10},
		  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
		  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/rogue/rogue"}
		}`

		fake := &fakeTemporalClient{}
		handler := newMux(newHandler(fake, func(string) string { return "" },
			WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, "")))

		recorder := postJSON(t, handler, "/api/runs", []byte(`{"config": `+configWithWebhook+`}`), nil)

		assert.Equal(t, http.StatusBadRequest, recorder.Code)
		assert.False(t, fake.executeCaptured)
	})

	t.Run("a client-supplied webhook is allowed on a dry-run submit", func(t *testing.T) {
		configWithWebhook := `{
		  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
		  "copies": 2,
		  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
		  "redundancy": {"targetPercentage": 10},
		  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
		  "delivery": {"webhookUrl": "https://discord.com/api/webhooks/123/abc"}
		}`

		fake := &fakeTemporalClient{executeRun: fakeWorkflowRun{workflowID: backup.WorkflowID, runID: "run-dry-webhook"}}
		handler := newMux(newHandler(fake, mhvtlEnv,
			WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, "")))

		recorder := postJSON(t, handler, "/api/runs", []byte(`{"config": `+configWithWebhook+`, "dryRun": true}`), nil)

		require.Equal(t, http.StatusCreated, recorder.Code)
	})

	t.Run("an invalid deploy override is rejected before Temporal contact", func(t *testing.T) {
		fake := &fakeTemporalClient{}
		handler := newMux(newHandler(fake, func(string) string { return "" },
			WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst0"}, "")))

		recorder := postJSON(t, handler, "/api/runs", []byte(`{"config": `+validSubmitConfigJSON+`}`), nil)

		assert.Equal(t, http.StatusBadRequest, recorder.Code)
		assert.False(t, fake.executeCaptured)
	})

	t.Run("deploy config overrides a burn-enabled run's optical burner drives", func(t *testing.T) {
		burnConfig := `{
		  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
		  "copies": 2,
		  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
		  "redundancy": {"targetPercentage": 10},
		  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
		  "delivery": {"opticalBurn": {"drives": ["/dev/rogue-burner"], "copies": 2}}
		}`

		fake := &fakeTemporalClient{executeRun: fakeWorkflowRun{workflowID: backup.WorkflowID, runID: "run-burn"}}
		handler := newMux(newHandler(fake, func(string) string { return "" },
			WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, ""),
			WithOpticalBurnerDrives([]string{"/dev/sr0", "/dev/sr1"})))

		recorder := postJSON(t, handler, "/api/runs", []byte(`{"config": `+burnConfig+`}`), nil)

		require.Equal(t, http.StatusCreated, recorder.Code)
		require.NotNil(t, fake.executeConfig)
		require.NotNil(t, fake.executeConfig.Delivery.OpticalBurn)
		assert.Equal(t, []string{"/dev/sr0", "/dev/sr1"}, fake.executeConfig.Delivery.OpticalBurn.Drives)
	})

	// copies:0 is not Enabled(), so the client drive path would otherwise survive.
	t.Run("client-supplied burner drives on a disabled burn block are rejected without a deploy burner", func(t *testing.T) {
		disabledBurnConfig := `{
		  "sources": [{"zfsPath": {"name": "bulk-pool-01/archive@snap"}}],
		  "copies": 2,
		  "library": {"changer": "/dev/sch0", "drives": ["/dev/nst0", "/dev/nst1"], "blankSlots": [1, 2], "tapeCapacityBytes": 2500000000000},
		  "redundancy": {"targetPercentage": 10},
		  "encryption": {"recipients": ["age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4"], "identity": "AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL"},
		  "delivery": {"opticalBurn": {"drives": ["/dev/rogue-burner"], "copies": 0}}
		}`

		fake := &fakeTemporalClient{}
		handler := newMux(newHandler(fake, func(string) string { return "" },
			WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, "")))

		recorder := postJSON(t, handler, "/api/runs", []byte(`{"config": `+disabledBurnConfig+`}`), nil)

		assert.Equal(t, http.StatusBadRequest, recorder.Code)
		assert.False(t, fake.executeCaptured)
	})

	t.Run("deploy burner drives add no opticalBurn block to a burn-off run", func(t *testing.T) {
		fake := &fakeTemporalClient{executeRun: fakeWorkflowRun{workflowID: backup.WorkflowID, runID: "run-noburn"}}
		handler := newMux(newHandler(fake, func(string) string { return "" },
			WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, ""),
			WithOpticalBurnerDrives([]string{"/dev/sr0", "/dev/sr1"})))

		recorder := postJSON(t, handler, "/api/runs", []byte(`{"config": `+validSubmitConfigJSON+`}`), nil)

		require.Equal(t, http.StatusCreated, recorder.Code)
		require.NotNil(t, fake.executeConfig)
		assert.Nil(t, fake.executeConfig.Delivery.OpticalBurn)
	})

	t.Run("a dry-run submission still honors the singleton conflict policy", func(t *testing.T) {
		fake := &fakeTemporalClient{executeRun: fakeWorkflowRun{workflowID: backup.WorkflowID, runID: "run-dry"}}
		handler := newMux(newHandler(fake, mhvtlEnv))

		postJSON(t, handler, "/api/runs", []byte(`{"config": `+validSubmitConfigJSON+`, "dryRun": true}`), nil)

		require.True(t, fake.executeCaptured)
		assert.True(t, fake.executeOptions.WorkflowExecutionErrorWhenAlreadyStarted)
		assert.Equal(t, backup.WorkflowID, fake.executeOptions.ID)
	})
}
