package backup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/solidDoWant/tape-archiver/pkg/tape"
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

// deliverAPIRecorder emulates the two Discord calls a successful Deliver makes
// against a webhook URL: the ?wait=true multipart POST that returns the created
// message ({id, channel_id}), and the follow-up GET on the webhook object that
// returns its guild ({guild_id}). It lets a test assert the full DeliverResult.
type deliverAPIRecorder struct {
	guildID   string
	channelID string
	messageID string
}

func (d *deliverAPIRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if req.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]string{"guild_id": d.guildID})

		return
	}

	_ = json.NewEncoder(w).Encode(map[string]string{"id": d.messageID, "channel_id": d.channelID})
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

	result, err := acts.Deliver(t.Context(), DeliverInput{
		WebhookURL: server.URL,
		ReportPath: writeReportArtifact(t),
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, []string{reportFileName}, recorder.names(),
		"exactly one file — the report — must be uploaded, and no ISO")
}

// TestDeliverCapturesPostedMessage covers issue #306: on a successful delivery the
// activity records the posted message's guild/channel/message identity (from the
// ?wait=true post response plus the webhook-object guild lookup) as its result, so
// the run overview can deep-link to the Discord message.
func TestDeliverCapturesPostedMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(&deliverAPIRecorder{guildID: "g1", channelID: "c1", messageID: "m1"})
	t.Cleanup(server.Close)

	var acts DeliverActivities

	result, err := acts.Deliver(t.Context(), DeliverInput{
		WebhookURL: server.URL,
		ReportPath: writeReportArtifact(t),
	})
	require.NoError(t, err)
	assert.Equal(t, &DeliverResult{GuildID: "g1", ChannelID: "c1", MessageID: "m1"}, result)
}

// TestDeliverGuildLookupFailureStillSucceeds covers issue #306's best-effort
// guarantee: when the report is delivered but the follow-up guild lookup fails, the
// activity still succeeds with the channel+message captured and an empty guild — a
// missing guild only omits the deep-link, it never turns a delivered report into a
// failed run.
func TestDeliverGuildLookupFailureStillSucceeds(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m1","channel_id":"c1"}`)
	}))
	t.Cleanup(server.Close)

	var acts DeliverActivities

	result, err := acts.Deliver(t.Context(), DeliverInput{
		WebhookURL: server.URL,
		ReportPath: writeReportArtifact(t),
	})
	require.NoError(t, err)
	assert.Equal(t, &DeliverResult{ChannelID: "c1", MessageID: "m1"}, result)
}

// TestDeliverEmptyWebhookNoOp covers AC3: a run with delivery disabled (empty
// webhook) succeeds without any upload.
func TestDeliverEmptyWebhookNoOp(t *testing.T) {
	t.Parallel()

	var acts DeliverActivities

	result, err := acts.Deliver(t.Context(), DeliverInput{
		WebhookURL: "",
		ReportPath: writeReportArtifact(t),
	})
	require.NoError(t, err)
	assert.Equal(t, &DeliverResult{}, result, "a disabled webhook yields an empty result — no link")
}

// TestDeliverReportFailureFails covers AC6: a failed report upload fails the
// activity and reports the report-upload error.
func TestDeliverReportFailureFails(t *testing.T) {
	t.Parallel()

	recorder := &uploadRecorder{status: http.StatusInternalServerError}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)

	var acts DeliverActivities

	_, err := acts.Deliver(t.Context(), DeliverInput{
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
		func(ctx context.Context, _ DeliverInput) (*DeliverResult, error) {
			gotHeartbeatTimeout = activity.GetInfo(ctx).HeartbeatTimeout

			return &DeliverResult{}, nil
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

			_, err := acts.Deliver(t.Context(), DeliverInput{
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
		Return(nil, temporal.NewNonRetryableApplicationError(
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

// TestDeliverySummary asserts the message posted with the report names the run and
// what it covers. Without it the report lands in Discord as an unlabelled PDF, so
// the channel says nothing about which run produced it or what is on the tapes
// until someone opens the attachment.
func TestDeliverySummary(t *testing.T) {
	t.Parallel()

	state := &runState{
		staged: []StagedArchive{
			{SourceIndex: 0, SizeBytes: 13_911_341_280},
			{SourceIndex: 1, SizeBytes: 332_412},
		},
		written: []WrittenTape{
			{Barcode: tape.Barcode("JCY631L5")},
			{Barcode: tape.Barcode("JCY632L5")},
		},
	}

	summary := deliverySummary("019fa67f-6e01-7752-b1b6-e1d2b7530cf5", state)

	assert.Contains(t, summary, "019fa67f-6e01-7752-b1b6-e1d2b7530cf5",
		"the message must name the run, so a reader can find it without opening the PDF")
	assert.Contains(t, summary, "2 archives")
	assert.Contains(t, summary, "13.9 GB", "the message must say how much was archived")
	assert.Contains(t, summary, "JCY631L5")
	assert.Contains(t, summary, "JCY632L5", "the message must name every tape written")

	// Discord's content limit; the message is short by construction but must
	// never be at risk of rejection for length.
	assert.Less(t, len(summary), 2000)
}

// TestDeliverySummaryPluralizesOneArchive covers the single-archive run, which is
// the common case for this project and must not read "1 archives".
func TestDeliverySummaryPluralizesOneArchive(t *testing.T) {
	t.Parallel()

	summary := deliverySummary("run-1", &runState{
		staged:  []StagedArchive{{SourceIndex: 0, SizeBytes: 1_500_000}},
		written: []WrittenTape{{Barcode: tape.Barcode("TAPE01L6")}},
	})

	assert.Contains(t, summary, "1 archive (1.5 MB)")
}

// TestHumanBytes pins the sizes the delivery message renders, in the decimal units
// tape and drive capacities are quoted in.
func TestHumanBytes(t *testing.T) {
	t.Parallel()

	tests := map[int64]string{
		0:                 "0 B",
		999:               "999 B",
		1_000:             "1.0 kB",
		1_500_000:         "1.5 MB",
		13_911_341_280:    "13.9 GB",
		2_500_000_000_000: "2.5 TB",
	}

	for bytes, want := range tests {
		assert.Equalf(t, want, humanBytes(bytes), "humanBytes(%d)", bytes)
	}
}
