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

// TestNotifyFailureActivity asserts the failure alert posts to the webhook, never
// returns an error (SPEC §11), and no-ops without a URL.
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
			require.NoError(t, err)

			if test.expectHit {
				assert.Equal(t, int32(1), hits.Load())
			} else {
				assert.Equal(t, int32(0), hits.Load())
			}
		})
	}
}

// TestNotifyBurnPauseActivity asserts the burn-pause alert is best-effort and
// always returns nil (SPEC §10, §11).
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
			require.NoError(t, err)

			if test.expectHit {
				assert.Equal(t, int32(1), hits.Load())
			} else {
				assert.Equal(t, int32(0), hits.Load())
			}
		})
	}
}

// TestWorkflowFailureSendsAlert asserts a phase failure fires the alert with run
// id, phase, and error, and surfaces the original error unmasked (SPEC §11).
func TestWorkflowFailureSendsAlert(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	// Empty plan, then fail at Report, which always dispatches its activity.
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

	require.ErrorContains(t, env.GetWorkflowError(), "phase Report")
	require.ErrorContains(t, env.GetWorkflowError(), "boom")

	assert.NotEmpty(t, captured.RunID)
	assert.Equal(t, PhaseReport, captured.Phase)
	assert.Contains(t, captured.ErrorSummary, "boom")
}

// TestFailureAlertErrorDoesNotMask asserts an alert delivery failure never
// replaces the run's original error (SPEC §11).
func TestFailureAlertErrorDoesNotMask(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

	// Fail at Report, as in TestWorkflowFailureSendsAlert.
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
