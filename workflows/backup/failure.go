package backup

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/webhook"
)

// failureAlertTimeout is short so an unreachable webhook cannot hold the run open.
const failureAlertTimeout = 30 * time.Second

const operatorPauseAlertTimeout = 30 * time.Second

// FailureInput is the failure-alert payload (SPEC §11).
type FailureInput struct {
	RunID string
	Phase string
	// ErrorSummary carries the error as text across the activity boundary.
	ErrorSummary string
}

// FailureActivities hosts the control-side operational alert activities (SPEC
// §11). An empty WebhookURL makes every alert a silent no-op.
type FailureActivities struct {
	// WebhookURL is DISCORD_FAILURE_WEBHOOK_URL.
	WebhookURL string
}

// NotifyFailure posts the failure alert. It always returns nil so delivery can
// never mask the run's original error.
func (a *FailureActivities) NotifyFailure(ctx context.Context, input FailureInput) error {
	webhook.New(a.WebhookURL).SendFailure(ctx, input.RunID, input.Phase, errors.New(input.ErrorSummary))

	return nil
}

// OperatorPauseInput is the Eject-pause alert payload (SPEC §4.3 phase 8).
type OperatorPauseInput struct {
	RunID string
	// ReadyForRemoval are the tapes now in the I/O station.
	ReadyForRemoval []string
	// Awaiting counts written tapes still to export.
	Awaiting int
}

// NotifyOperatorPause posts the I/O-station-full pause alert. It is best-effort
// so a webhook outage never aborts a run that is only waiting.
func (a *FailureActivities) NotifyOperatorPause(ctx context.Context, input OperatorPauseInput) error {
	webhook.New(a.WebhookURL).SendOperatorPause(ctx, input.RunID, input.ReadyForRemoval, input.Awaiting)

	return nil
}

// WritePathPauseInput is the Load/Write-failure pause alert payload.
type WritePathPauseInput struct {
	RunID string
	// Phase is "Load" or "Write".
	Phase string
	// AffectedTapes are tapes to replace with blanks; empty for a Load failure.
	AffectedTapes []string
	// ReloadSlots are the slots to restock; resume loads from these same slots.
	ReloadSlots  []int
	ErrorSummary string
}

// NotifyWritePathPause posts the Load/Write-failure pause alert, best-effort.
func (a *FailureActivities) NotifyWritePathPause(ctx context.Context, input WritePathPauseInput) error {
	webhook.New(a.WebhookURL).SendWritePathPause(ctx, input.RunID, input.Phase, input.AffectedTapes, input.ReloadSlots, input.ErrorSummary)

	return nil
}

// BurnPauseInput is the Burn-pause alert payload (SPEC §10).
type BurnPauseInput struct {
	RunID string
	// Devices are burners needing a fresh blank disc.
	Devices []string
	// ErrorSummary is the burn/verify failure or the disc-swap prompt.
	ErrorSummary string
}

// NotifyBurnPause posts the Burn pause alert, best-effort.
func (a *FailureActivities) NotifyBurnPause(ctx context.Context, input BurnPauseInput) error {
	webhook.New(a.WebhookURL).SendBurnPause(ctx, input.RunID, input.Devices, input.ErrorSummary)

	return nil
}

// notifyFailure fires the failure alert on a disconnected context, so it also
// fires on cancellation, and never propagates an error that could mask runErr.
func notifyFailure(ctx workflow.Context, failingPhase string, runErr error) {
	input := FailureInput{
		RunID:        workflow.GetInfo(ctx).WorkflowExecution.ID,
		Phase:        failingPhase,
		ErrorSummary: runErr.Error(),
	}

	disconnected, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()

	// The control worker owns the env-configured failure webhook.
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
