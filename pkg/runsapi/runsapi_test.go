package runsapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// fakeEncodedValue is a minimal converter.EncodedValue standing in for the
// real SDK payload decoding, so QueryWorkflow can be faked without a real
// Temporal connection.
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

// fakeTemporalClient is a hand-rolled fake of the TemporalClient interface —
// exercising handler logic against public HTTP behavior without a real
// Temporal connection, per the "never mock the component under test" rule
// (this fakes a dependency, not runsapi itself).
type fakeTemporalClient struct {
	listResponse *workflowservice.ListWorkflowExecutionsResponse
	listErr      error

	describeResponse *workflowservice.DescribeWorkflowExecutionResponse
	describeErr      error

	queryResult interface{}
	queryErr    error
}

func (f *fakeTemporalClient) ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	return f.listResponse, f.listErr
}

func (f *fakeTemporalClient) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return f.describeResponse, f.describeErr
}

func (f *fakeTemporalClient) QueryWorkflow(context.Context, string, string, string, ...interface{}) (converter.EncodedValue, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}

	return fakeEncodedValue{value: f.queryResult}, nil
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
		// Sorting is done in Go (see listRuns' doc comment), not via "ORDER
		// BY" in the query: Temporal's standard (non-Elasticsearch)
		// visibility store rejects that clause as unsupported.
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

// spyClient wraps a TemporalClient and records the ListWorkflow request, so
// the newest-first-ordering test can assert on the query sent without a fake
// hand-coding request capture itself.
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
		name       string
		runID      string
		client     *fakeTemporalClient
		wantStatus int
		wantPhase  string
		errAssert  require.ErrorAssertionFunc
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
			name:  "a failed phase query does not fail the request; phase reports empty",
			runID: "run-1",
			client: &fakeTemporalClient{
				describeResponse: &workflowservice.DescribeWorkflowExecutionResponse{
					WorkflowExecutionInfo: executionInfo("run-1", enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, start, nil),
				},
				queryErr: assertError{"no worker polling"},
			},
			wantStatus: http.StatusOK,
			wantPhase:  "",
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
				errAssert(t, nil)

				return
			}

			errAssert(t, decodeAPIError(t, recorder))
		})
	}
}

// decodeAPIError decodes a non-2xx response's JSON error body into an error,
// so table-driven tests can assert on it with a require.ErrorAssertionFunc
// like any other error-returning call, per this repo's testing style.
func decodeAPIError(t *testing.T, recorder *httptest.ResponseRecorder) error {
	t.Helper()

	var body errorResponse

	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.NotEmpty(t, body.Error)

	return errors.New(body.Error)
}

// assertError is a minimal error type distinct from any serviceerror, so
// tests can assert on error-wrapping behavior without depending on gRPC
// status construction.
type assertError struct{ msg string }

func (e assertError) Error() string { return e.msg }
