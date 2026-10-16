package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

const statusTestRunID = "run-430"

var errNoWorker = errors.New("no poller seen for task queue")

// fakeEncodedValue decodes a fixed string query result.
type fakeEncodedValue struct{ value string }

func (f fakeEncodedValue) HasValue() bool { return true }

func (f fakeEncodedValue) Get(valuePtr interface{}) error {
	pointer, ok := valuePtr.(*string)
	if !ok {
		return errors.New("unexpected value type")
	}

	*pointer = f.value

	return nil
}

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
	f.index++

	if f.err != nil {
		return nil, f.err
	}

	return f.events[f.index-1], nil
}

// fakeStatusClient serves a running backup whose phase query and history can
// each be made to fail.
type fakeStatusClient struct {
	phase      string
	queryErr   error
	events     []*historypb.HistoryEvent
	historyErr error
}

func (f *fakeStatusClient) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{
			Execution: &commonpb.WorkflowExecution{WorkflowId: backupWorkflowID, RunId: statusTestRunID},
			Status:    enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		},
	}, nil
}

func (f *fakeStatusClient) QueryWorkflow(_ context.Context, _, runID, queryType string, _ ...interface{}) (converter.EncodedValue, error) {
	if runID != statusTestRunID || queryType != backup.LastCompletedPhaseQuery {
		return nil, errors.New("unexpected query")
	}

	if f.queryErr != nil {
		return nil, f.queryErr
	}

	return fakeEncodedValue{value: f.phase}, nil
}

func (f *fakeStatusClient) GetWorkflowHistory(_ context.Context, _, runID string, _ bool, _ enumspb.HistoryEventFilterType) client.HistoryEventIterator {
	if runID != statusTestRunID {
		return &fakeHistoryIterator{err: errors.New("unexpected run ID")}
	}

	return &fakeHistoryIterator{events: f.events, err: f.historyErr}
}

var statusTestStart = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// preparingHistory is an open run that finished Resolve and is in Prepare.
func preparingHistory() []*historypb.HistoryEvent {
	at := func(minutes int) *timestamppb.Timestamp {
		return timestamppb.New(statusTestStart.Add(time.Duration(minutes) * time.Minute))
	}

	scheduled := func(id int64, minutes int, name string) *historypb.HistoryEvent {
		return &historypb.HistoryEvent{
			EventId:   id,
			EventTime: at(minutes),
			EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED,
			Attributes: &historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{
				ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{
					ActivityType: &commonpb.ActivityType{Name: name},
				},
			},
		}
	}

	return []*historypb.HistoryEvent{
		{
			EventId:   1,
			EventTime: at(0),
			EventType: enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED,
			Attributes: &historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{
				WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{},
			},
		},
		scheduled(2, 1, "ResolveAndCheck"),
		{
			EventId:   3,
			EventTime: at(2),
			EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED,
			Attributes: &historypb.HistoryEvent_ActivityTaskCompletedEventAttributes{
				ActivityTaskCompletedEventAttributes: &historypb.ActivityTaskCompletedEventAttributes{ScheduledEventId: 2},
			},
		},
		scheduled(4, 3, "PrepareArchives"),
	}
}

func TestWriteStatus(t *testing.T) {
	const preparing = "Prepare (running since 2026-10-01T12:03:00Z)"

	tests := []struct {
		name              string
		client            *fakeStatusClient
		wantLastCompleted string
		wantCurrent       string
	}{
		{
			name:              "live query answers",
			client:            &fakeStatusClient{phase: backup.PhaseVerify, events: preparingHistory()},
			wantLastCompleted: backup.PhaseVerify,
			wantCurrent:       preparing,
		},
		{
			name:              "query fails, history derives the phase",
			client:            &fakeStatusClient{queryErr: errNoWorker, events: preparingHistory()},
			wantLastCompleted: backup.PhaseResolve,
			wantCurrent:       preparing,
		},
		{
			name:              "query and history both fail",
			client:            &fakeStatusClient{queryErr: errNoWorker, historyErr: errors.New("history unavailable")},
			wantLastCompleted: phaseUnavailable,
			wantCurrent:       phaseUnavailable,
		},
		{
			name:              "query answers no phase yet, history fails",
			client:            &fakeStatusClient{historyErr: errors.New("history unavailable")},
			wantLastCompleted: phaseNone,
			wantCurrent:       phaseUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, writeStatus(t.Context(), test.client, &out))

			want := "Workflow:             " + backupWorkflowID + "\n" +
				"Status:               Running\n" +
				"Last completed phase: " + test.wantLastCompleted + "\n" +
				"Current phase:        " + test.wantCurrent + "\n"
			assert.Equal(t, want, out.String())
		})
	}
}
