package backup

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/webhook"
)

// failureAlertTimeout bounds the failure-alert activity. The alert is a single
// best-effort webhook POST, so a short timeout is appropriate: a slow or
// unreachable webhook must not hold the run open.
const failureAlertTimeout = 30 * time.Second

// operatorPauseAlertTimeout bounds the operator-pause alert activity. Like the
// failure alert it is a single best-effort webhook POST.
const operatorPauseAlertTimeout = 30 * time.Second

// FailureInput is the payload for the failure-alert activity. It carries exactly
// what SPEC §11 requires in the alert: the run id, the failing phase, and the
// error summary. Partial context (e.g. tapes already written and needing manual
// handling) is added by the Write/Eject sub-issues once that state exists.
type FailureInput struct {
	// RunID is the workflow (run) id, so an operator can correlate the alert
	// with the run in Temporal and in logs.
	RunID string
	// Phase is the name of the phase that was in flight when the run failed.
	Phase string
	// ErrorSummary is the run failure rendered as text; the error itself does
	// not cross the activity boundary, so it is carried as its message.
	ErrorSummary string
}

// FailureActivities hosts the operational failure-alert activity (SPEC §11). It
// is constructed on the control worker with the DISCORD_FAILURE_WEBHOOK_URL from
// the environment; an empty URL yields a disabled webhook client, so alerting is
// a silent no-op when the variable is unset.
type FailureActivities struct {
	// WebhookURL is the Discord failure webhook (DISCORD_FAILURE_WEBHOOK_URL).
	// Empty disables alerting.
	WebhookURL string
}

// NotifyFailure posts the operational failure alert. Per SPEC §11 it must never
// mask the run's original error: pkg/webhook.SendFailure logs a delivery failure
// rather than returning it, so this activity always returns nil. When the
// webhook URL is empty the client is a no-op and nothing is sent.
func (a *FailureActivities) NotifyFailure(ctx context.Context, input FailureInput) error {
	webhook.New(a.WebhookURL).SendFailure(ctx, input.RunID, input.Phase, errors.New(input.ErrorSummary))

	return nil
}

// OperatorPauseInput is the payload for the operator-pause alert activity. It
// carries what an operator needs to act on the Eject-phase pause (SPEC §4.3
// phase 8, §11): the run id, the tapes now sitting in the import/export station
// ready for removal, and how many written tapes still await a free slot.
type OperatorPauseInput struct {
	// RunID is the workflow (run) id, so an operator can correlate the alert with
	// the run.
	RunID string
	// ReadyForRemoval lists the barcodes of the tapes currently in I/O-station
	// slots — the tapes the operator should remove to free slots.
	ReadyForRemoval []string
	// Awaiting is the count of written tapes still to be exported once slots free.
	Awaiting int
}

// NotifyOperatorPause posts the operator-in-the-loop pause alert when the Eject
// phase fills the import/export station (SPEC §4.3 phase 8, §11). It uses the
// same operational webhook as NotifyFailure and is best-effort: a delivery
// failure is logged by pkg/webhook, not returned, so a webhook outage never
// aborts a run that is only waiting for the operator. When the webhook URL is
// empty the client is a no-op and nothing is sent.
func (a *FailureActivities) NotifyOperatorPause(ctx context.Context, input OperatorPauseInput) error {
	webhook.New(a.WebhookURL).SendOperatorPause(ctx, input.RunID, input.ReadyForRemoval, input.Awaiting)

	return nil
}

// notifyFailure runs the failure-alert activity from the workflow when a run
// fails (SPEC §11). It executes on a disconnected context so the alert still
// fires when the workflow itself is cancelled, and it never propagates an error:
// an activity-dispatch failure is logged, leaving the run's original error to
// surface unmasked in the caller.
func notifyFailure(ctx workflow.Context, failingPhase string, runErr error) {
	input := FailureInput{
		RunID:        workflow.GetInfo(ctx).WorkflowExecution.ID,
		Phase:        failingPhase,
		ErrorSummary: runErr.Error(),
	}

	// A disconnected context is not cancelled when the workflow is, so the alert
	// is delivered even on cancellation (SPEC §11).
	disconnected, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()

	// The operational failure alert is delivered by the control worker (it uses
	// the env-configured failure webhook, distinct from the data-side success
	// delivery), so it runs on the control task queue.
	actx := workflow.WithActivityOptions(disconnected, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: failureAlertTimeout,
	})

	var activities *FailureActivities
	if alertErr := workflow.ExecuteActivity(actx, activities.NotifyFailure, input).Get(actx, nil); alertErr != nil {
		workflow.GetLogger(ctx).Error("failed to deliver failure alert",
			"phase", failingPhase,
			"error", alertErr,
		)
	}
}
