package backup

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
