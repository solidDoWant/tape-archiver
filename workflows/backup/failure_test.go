package backup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestNotifyFailureActivity covers the failure-alert activity in isolation: it
// posts to the configured webhook, never returns an error (a delivery failure is
// swallowed so it cannot mask the run's original error, SPEC §11), and is a
// silent no-op when no webhook URL is configured.
func TestNotifyFailureActivity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		emptyURL  bool
		expectHit bool
	}{
		{name: "delivers alert on 2xx", status: http.StatusNoContent, expectHit: true},
		{name: "swallows delivery error on non-2xx", status: http.StatusInternalServerError, expectHit: true},
		{name: "no-op when URL unset", emptyURL: true, expectHit: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(test.status)
			}))
			t.Cleanup(server.Close)

			url := server.URL
			if test.emptyURL {
				url = ""
			}

			activities := &FailureActivities{WebhookURL: url}

			err := activities.NotifyFailure(t.Context(), FailureInput{
				RunID:        "backup-1",
				Phase:        PhaseWrite,
				ErrorSummary: "phase Write: boom",
			})
			// SendFailure never returns a delivery error, so the activity always
			// succeeds regardless of the webhook's response.
			require.NoError(t, err)

			if test.expectHit {
				assert.Equal(t, int32(1), hits.Load())
			} else {
				assert.Equal(t, int32(0), hits.Load())
			}
		})
	}
}

// TestNotifyBurnPauseActivity asserts the burn-pause alert activity is best-effort
// like the other operator alerts: it delivers on a healthy webhook, swallows a
// delivery error, and is a no-op when the webhook URL is unset — always returning
// nil so a webhook outage never aborts a paused burn (SPEC §10, §11).
func TestNotifyBurnPauseActivity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		emptyURL  bool
		expectHit bool
	}{
		{name: "delivers alert on 2xx", status: http.StatusNoContent, expectHit: true},
		{name: "swallows delivery error on non-2xx", status: http.StatusInternalServerError, expectHit: true},
		{name: "no-op when URL unset", emptyURL: true, expectHit: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(test.status)
			}))
			t.Cleanup(server.Close)

			url := server.URL
			if test.emptyURL {
				url = ""
			}

			activities := &FailureActivities{WebhookURL: url}

			err := activities.NotifyBurnPause(t.Context(), BurnPauseInput{
				RunID:        "backup-1",
				Devices:      []string{"/dev/sr0"},
				ErrorSummary: "a burn or verify failed: drive reported a write error",
			})
			// SendBurnPause never returns a delivery error, so the activity always
			// succeeds regardless of the webhook's response.
			require.NoError(t, err)

			if test.expectHit {
				assert.Equal(t, int32(1), hits.Load())
			} else {
				assert.Equal(t, int32(0), hits.Load())
			}
		})
	}
}

// TestWorkflowFailureSendsAlert asserts that when a phase fails the deferred
// handler fires the failure-alert activity with the run id, failing phase, and
// error summary, while the run's original error surfaces unmasked (SPEC §11).
func TestWorkflowFailureSendsAlert(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	// Resolve to an empty plan so every phase before Report no-ops on a valid
	// config, then fail at the Report phase: it always dispatches its activity
	// regardless of the plan content, making it reliable for exercising the
	// deferred failure-alert handler.
	expectResolveEmpty(env)

	env.OnActivity((&ReportActivities{}).BuildReport, mock.Anything, mock.Anything).
		Return(ReportOutput{}, errors.New("boom"))

	var captured FailureInput

	env.OnActivity((&FailureActivities{}).NotifyFailure, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input FailureInput) error {
			captured = input

			return nil
		})

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())

	// The original failure surfaces unmasked: the workflow error names the
	// failing phase and carries the underlying error.
	require.ErrorContains(t, env.GetWorkflowError(), "phase Report")
	require.ErrorContains(t, env.GetWorkflowError(), "boom")

	// The alert carries the run id, the failing phase, and the error summary.
	assert.NotEmpty(t, captured.RunID)
	assert.Equal(t, PhaseReport, captured.Phase)
	assert.Contains(t, captured.ErrorSummary, "boom")
}

// TestFailureAlertErrorDoesNotMask asserts that a failure of the alert delivery
// itself never replaces the run's original error (SPEC §11): the workflow error
// remains the phase failure, not the alert error.
func TestFailureAlertErrorDoesNotMask(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

	// Fail at the Report phase — same rationale as TestWorkflowFailureSendsAlert.
	expectResolveEmpty(env)

	env.OnActivity((&ReportActivities{}).BuildReport, mock.Anything, mock.Anything).
		Return(ReportOutput{}, errors.New("boom"))

	env.OnActivity((&FailureActivities{}).NotifyFailure, mock.Anything, mock.Anything).
		Return(errors.New("alert delivery failed"))

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())

	workflowErr := env.GetWorkflowError()
	require.ErrorContains(t, workflowErr, "phase Report")
	require.ErrorContains(t, workflowErr, "boom")
	assert.NotContains(t, workflowErr.Error(), "alert delivery failed")
}
