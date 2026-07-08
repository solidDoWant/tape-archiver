package backup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// uploadRecorder is an httptest handler that records the base filename of each
// multipart upload it receives.
type uploadRecorder struct {
	mu      sync.Mutex
	uploads []string
	status  int
}

func (r *uploadRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, header, err := req.FormFile("files[0]")

	r.mu.Lock()
	if err == nil {
		r.uploads = append(r.uploads, header.Filename)
	}
	r.mu.Unlock()

	status := r.status
	if status == 0 {
		status = http.StatusOK
	}

	w.WriteHeader(status)
}

func (r *uploadRecorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.uploads...)
}

// writeReportArtifact stages a dummy report and returns its path.
func writeReportArtifact(t *testing.T) (reportPath string) {
	t.Helper()

	reportPath = filepath.Join(t.TempDir(), reportFileName)
	require.NoError(t, os.WriteFile(reportPath, []byte("%PDF-1.4 report"), 0o644))

	return reportPath
}

// TestDeliverUploadsReportOnly covers AC1/AC2: exactly one file — the report — is
// uploaded to the configured webhook, and no recovery ISO is.
func TestDeliverUploadsReportOnly(t *testing.T) {
	t.Parallel()

	recorder := &uploadRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)

	var acts DeliverActivities

	err := acts.Deliver(t.Context(), DeliverInput{
		WebhookURL: server.URL,
		ReportPath: writeReportArtifact(t),
	})
	require.NoError(t, err)

	assert.Equal(t, []string{reportFileName}, recorder.names(),
		"exactly one file — the report — must be uploaded, and no ISO")
}

// TestDeliverEmptyWebhookNoOp covers AC3: a run with delivery disabled (empty
// webhook) succeeds without any upload.
func TestDeliverEmptyWebhookNoOp(t *testing.T) {
	t.Parallel()

	var acts DeliverActivities

	err := acts.Deliver(t.Context(), DeliverInput{
		WebhookURL: "",
		ReportPath: writeReportArtifact(t),
	})
	require.NoError(t, err)
}

// TestDeliverReportFailureFails covers AC6: a failed report upload fails the
// activity and reports the report-upload error.
func TestDeliverReportFailureFails(t *testing.T) {
	t.Parallel()

	recorder := &uploadRecorder{status: http.StatusInternalServerError}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)

	var acts DeliverActivities

	err := acts.Deliver(t.Context(), DeliverInput{
		WebhookURL: server.URL,
		ReportPath: writeReportArtifact(t),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deliver report")
}

// deliverPhaseTestWorkflow drives deliverPhase so its activity scheduling options
// can be inspected in the test env.
func deliverPhaseTestWorkflow(ctx workflow.Context, cfg config.Config) error {
	return deliverPhase(ctx, cfg, &runState{reportPath: "/stage/report.pdf"})
}

// TestDeliverPhaseSetsHeartbeatTimeout covers AC2: the Deliver activity is
// scheduled with the shared data-activity HeartbeatTimeout, so a data worker that
// dies hard mid-Deliver is detected within that window (2 min) rather than only
// after the 30-minute deliverTimeout. Temporal enforces the detection window from
// the activity's HeartbeatTimeout, so asserting it is set is the observable that
// guarantees the fast detection.
func TestDeliverPhaseSetsHeartbeatTimeout(t *testing.T) {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(deliverPhaseTestWorkflow)
	env.RegisterActivity(newDeliverActivities())

	var gotHeartbeatTimeout time.Duration

	env.OnActivity((&DeliverActivities{}).Deliver, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, _ DeliverInput) error {
			gotHeartbeatTimeout = activity.GetInfo(ctx).HeartbeatTimeout

			return nil
		})

	env.ExecuteWorkflow(deliverPhaseTestWorkflow, config.Config{})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, activityHeartbeatTimeout, gotHeartbeatTimeout,
		"Deliver must carry the data-activity HeartbeatTimeout so a dead worker is detected within minutes, not after the 30-minute deliverTimeout")
}

// flakyUpload is an httptest handler that fails the first failFirst uploads with
// failStatus, then succeeds, recording every upload's filename and total hits. A
// large failFirst models a webhook that never recovers.
type flakyUpload struct {
	mu        sync.Mutex
	hits      int
	uploads   []string
	failFirst int
	status    int
}

func (f *flakyUpload) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	_, header, err := req.FormFile("files[0]")

	f.mu.Lock()
	if err == nil {
		f.uploads = append(f.uploads, header.Filename)
	}

	f.hits++
	n := f.hits
	f.mu.Unlock()

	if n <= f.failFirst {
		w.WriteHeader(f.status)

		return
	}

	w.WriteHeader(http.StatusOK)
}

func (f *flakyUpload) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.hits
}

// TestDeliverDeterministicRejectionIsNonRetryable covers AC1: a deterministic
// webhook rejection (a permanent 4xx — deleted/rotated webhook, or an oversize
// report) fails the Deliver activity with a non-retryable ApplicationError, so the
// run fails fast rather than retrying the identical upload forever. A transient
// status (5xx/429) stays retryable so a brief outage can recover.
func TestDeliverDeterministicRejectionIsNonRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		status           int
		wantNonRetryable bool
	}{
		{name: "404 deleted webhook", status: http.StatusNotFound, wantNonRetryable: true},
		{name: "401 rotated webhook", status: http.StatusUnauthorized, wantNonRetryable: true},
		{name: "400 rejected payload", status: http.StatusBadRequest, wantNonRetryable: true},
		{name: "413 report too large", status: http.StatusRequestEntityTooLarge, wantNonRetryable: true},
		{name: "429 rate limited is transient", status: http.StatusTooManyRequests, wantNonRetryable: false},
		{name: "503 outage is transient", status: http.StatusServiceUnavailable, wantNonRetryable: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			recorder := &uploadRecorder{status: test.status}
			server := httptest.NewServer(recorder)
			t.Cleanup(server.Close)

			var acts DeliverActivities

			err := acts.Deliver(t.Context(), DeliverInput{
				WebhookURL: server.URL,
				ReportPath: writeReportArtifact(t),
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "deliver report")

			var appErr *temporal.ApplicationError
			if test.wantNonRetryable {
				require.ErrorAs(t, err, &appErr,
					"a deterministic rejection must be a non-retryable ApplicationError so the run fails fast")
				assert.True(t, appErr.NonRetryable(), "the rejection must be non-retryable")
				assert.Equal(t, deliverWebhookRejectedErrorType, appErr.Type())
			} else {
				assert.False(t, errors.As(err, &appErr),
					"a transient status must stay a retryable error, not a non-retryable ApplicationError")
			}
		})
	}
}

// deliverPhaseUploadWorkflow drives the real deliverPhase (RetryPolicy and all)
// against a live webhook so the bounded-retry and recovery behavior is observable.
func deliverPhaseUploadWorkflow(ctx workflow.Context, cfg config.Config, reportPath string) error {
	return deliverPhase(ctx, cfg, &runState{reportPath: reportPath})
}

// newDeliverUploadEnv builds a test workflow env with the real Deliver activity
// registered (unmocked), so deliverPhase's RetryPolicy governs a real upload.
func newDeliverUploadEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()

	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(deliverPhaseUploadWorkflow)
	env.RegisterActivity(newDeliverActivities())

	return env
}

// TestDeliverBoundedRetryTerminates covers AC1: a webhook that never recovers
// (persistent transient failure) fails the run after a bounded number of attempts
// (deliverMaxAttempts) rather than retrying indefinitely — Deliver is the final
// phase, so an unbounded retry would wedge the run silently.
func TestDeliverBoundedRetryTerminates(t *testing.T) {
	t.Parallel()

	handler := &flakyUpload{failFirst: 1_000_000, status: http.StatusServiceUnavailable}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	env := newDeliverUploadEnv(t)
	cfg := config.Config{Delivery: config.Delivery{WebhookURL: server.URL}}

	env.ExecuteWorkflow(deliverPhaseUploadWorkflow, cfg, writeReportArtifact(t))

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError(), "a webhook that never recovers must fail the run, not retry forever")
	assert.Equal(t, deliverMaxAttempts, handler.count(),
		"the upload must be attempted a bounded number of times, not indefinitely")
}

// TestDeliverTransientFailureRecovers covers AC3: a webhook that fails transiently
// and then recovers (a brief outage) is retried and the run completes successfully.
func TestDeliverTransientFailureRecovers(t *testing.T) {
	t.Parallel()

	// Fail the first two attempts with a 503, then succeed — within the
	// deliverMaxAttempts budget.
	handler := &flakyUpload{failFirst: 2, status: http.StatusServiceUnavailable}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	env := newDeliverUploadEnv(t)
	cfg := config.Config{Delivery: config.Delivery{WebhookURL: server.URL}}

	env.ExecuteWorkflow(deliverPhaseUploadWorkflow, cfg, writeReportArtifact(t))

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "a transient outage that recovers must let the run complete")
	assert.Equal(t, 3, handler.count(), "the upload must be retried until it succeeds on the third attempt")
}

// TestDeliverFailureAlertNamesDeliver covers AC2: when the Deliver phase fails with
// a bounded (non-retryable) error, the workflow ends and the operational failure
// alert fires naming the Deliver phase (SPEC §11) — the run no longer wedges
// silently after all tapes are written.
func TestDeliverFailureAlertNamesDeliver(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

	// Resolve to an empty plan so every phase before Report no-ops, then let Report
	// succeed and fail Deliver with the deterministic-rejection error the real
	// activity produces for a permanent 4xx.
	expectResolveEmpty(env)
	env.OnActivity((&ReportActivities{}).BuildReport, mock.Anything, mock.Anything).
		Return(ReportOutput{}, nil)
	env.OnActivity((&DeliverActivities{}).Deliver, mock.Anything, mock.Anything).
		Return(temporal.NewNonRetryableApplicationError(
			`deliver report "report.pdf": webhook: unexpected status 404`,
			deliverWebhookRejectedErrorType, nil))

	var captured FailureInput

	env.OnActivity((&FailureActivities{}).NotifyFailure, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input FailureInput) error {
			captured = input

			return nil
		})

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())

	// The run ends with an error naming the Deliver phase, unmasked.
	require.ErrorContains(t, env.GetWorkflowError(), "phase "+PhaseDeliver)

	// The failure alert names the Deliver phase.
	assert.Equal(t, PhaseDeliver, captured.Phase)
	assert.NotEmpty(t, captured.RunID)
}
