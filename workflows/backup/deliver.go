package backup

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/webhook"
)

// The Deliver phase (SPEC §4.3 phase 11) is the run's final step: it uploads the
// PDF report the Report phase built to the Discord webhook named in the run config
// (SPEC §11 success delivery). The recovery ISO is no longer delivered — the burned
// recovery disc is its durable home (SPEC §10). This is the per-run success path,
// distinct from the operational failure alert (which is env-configured and fires
// from the workflow's deferred handler). It runs on the data worker, where the
// report lives, so its bytes never cross a Temporal payload.
//
// When the run config names no webhook the webhook client is a no-op, so a run
// with delivery disabled still completes successfully.

// deliverTimeout bounds the Deliver activity: a single HTTP multipart upload of the
// PDF report. Generous, while still bounding a stuck upload.
const deliverTimeout = 30 * time.Minute

// DeliverActivities hosts the data-side Deliver activity. It carries no
// dependencies: the webhook target is per-run config, passed in the input.
type DeliverActivities struct{}

// newDeliverActivities returns the data-side Deliver activity.
func newDeliverActivities() *DeliverActivities { return &DeliverActivities{} }

// DeliverInput is the payload for the Deliver activity: the webhook target and the
// staged report path produced by the Report phase.
type DeliverInput struct {
	// WebhookURL is the run config's Discord success webhook (SPEC §5 Delivery).
	// Empty disables delivery — the activity is then a no-op.
	WebhookURL string
	// ReportPath is the staged PDF report to upload.
	ReportPath string
}

// Deliver uploads the report to the configured Discord webhook (SPEC §4.3 phase 11,
// §11). A non-2xx response or transport error fails the activity (and Temporal
// retries it); an empty webhook URL makes the upload a no-op.
func (a *DeliverActivities) Deliver(ctx context.Context, input DeliverInput) error {
	client := webhook.New(input.WebhookURL)

	if err := client.SendFile(ctx, input.ReportPath); err != nil {
		return fmt.Errorf("deliver report %q: %w", input.ReportPath, err)
	}

	return nil
}

// deliverPhase orchestrates the Deliver phase (SPEC §4.3 phase 11): it runs the
// data-side Deliver activity with the webhook target and the report path the Report
// phase recorded in runState.
func deliverPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: deliverTimeout,
	})

	var activities *DeliverActivities

	input := DeliverInput{
		WebhookURL: cfg.Delivery.WebhookURL,
		ReportPath: state.reportPath,
	}

	return workflow.ExecuteActivity(dataCtx, activities.Deliver, input).Get(dataCtx, nil)
}
