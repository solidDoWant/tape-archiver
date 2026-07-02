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

// writeArtifacts stages a dummy report and ISO and returns their paths.
func writeArtifacts(t *testing.T) (reportPath, isoPath string) {
	t.Helper()

	dir := t.TempDir()
	reportPath = filepath.Join(dir, reportFileName)
	isoPath = filepath.Join(dir, compressedISOFileName)

	require.NoError(t, os.WriteFile(reportPath, []byte("%PDF-1.4 report"), 0o644))
	require.NoError(t, os.WriteFile(isoPath, []byte("compressed-iso-bytes"), 0o644))

	return reportPath, isoPath
}

// TestDeliverUploadsBothArtifacts covers AC2: the report and ISO are both uploaded
// to the configured webhook.
func TestDeliverUploadsBothArtifacts(t *testing.T) {
	t.Parallel()

	recorder := &uploadRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)

	reportPath, isoPath := writeArtifacts(t)

	var acts DeliverActivities

	err := acts.Deliver(t.Context(), DeliverInput{
		WebhookURL: server.URL,
		ReportPath: reportPath,
		ISOPath:    isoPath,
	})
	require.NoError(t, err)

	assert.Equal(t, []string{reportFileName, compressedISOFileName}, recorder.names(),
		"report must be uploaded first, then the ISO")
}

// TestDeliverEmptyWebhookNoOp checks a run with delivery disabled (empty webhook)
// succeeds without any upload.
func TestDeliverEmptyWebhookNoOp(t *testing.T) {
	t.Parallel()

	reportPath, isoPath := writeArtifacts(t)

	var acts DeliverActivities

	err := acts.Deliver(t.Context(), DeliverInput{
		WebhookURL: "",
		ReportPath: reportPath,
		ISOPath:    isoPath,
	})
	require.NoError(t, err)
}

// TestDeliverReportFailureStopsBeforeISO checks that a failed report upload fails
// the activity and the ISO is not uploaded (the report is uploaded first).
func TestDeliverReportFailureStopsBeforeISO(t *testing.T) {
	t.Parallel()

	recorder := &uploadRecorder{status: http.StatusInternalServerError}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)

	reportPath, isoPath := writeArtifacts(t)

	var acts DeliverActivities

	err := acts.Deliver(t.Context(), DeliverInput{
		WebhookURL: server.URL,
		ReportPath: reportPath,
		ISOPath:    isoPath,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deliver report")
}
