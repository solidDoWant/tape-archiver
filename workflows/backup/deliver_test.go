package backup

import (
	"context"
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
