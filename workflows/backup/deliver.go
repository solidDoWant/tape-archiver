package backup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/webhook"
)

// The Deliver phase (SPEC §4.3 phase 11) is the run's final step: it uploads the
// PDF report the Report phase built to the Discord webhook named in the run config
// (SPEC §11 success delivery). The report is the sole delivered artifact; the
// recovery ISO travels on its burned disc (SPEC §10). This is the per-run success
// path, distinct from the operational failure alert (which is env-configured and
// fires from the workflow's deferred handler). It runs on the data worker, where the
// report lives, so its bytes never cross a Temporal payload.
//
// When the run config names no webhook the webhook client is a no-op, so a run
// with delivery disabled still completes successfully.

// deliverTimeout bounds the Deliver activity: a single HTTP multipart upload of the
// PDF report. Generous, while still bounding a stuck upload.
const deliverTimeout = 30 * time.Minute

// deliverMaxAttempts bounds how many times a *transient* Deliver failure (a 429/5xx
// from Discord, or a transport blip) is retried before the run gives up. Deliver is
// the final phase, so without a bound a webhook that never recovers would loop under
// Temporal's server-default (unlimited) policy and the run would neither complete nor
// fail — the SPEC §11 failure alert would never fire. A small bound lets a brief
// outage recover while still terminating a persistently failing upload. Deterministic
// failures (a permanent 4xx) short-circuit this budget: classifyDeliverError marks
// them non-retryable so they fail on the first attempt.
const deliverMaxAttempts = 5

// deliverWebhookRejectedErrorType is the ApplicationError type for a deterministic
// webhook rejection (a permanent 4xx: deleted/rotated webhook, or an oversize report).
const deliverWebhookRejectedErrorType = "deliver-webhook-rejected"

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
// §11). A transient failure (a 429/5xx response or a transport error) fails the
// activity and Temporal retries it up to deliverMaxAttempts; a deterministic
// rejection (a permanent 4xx — a deleted/rotated webhook or an oversize report) is
// returned non-retryable so the run fails fast and the failure alert fires. An empty
// webhook URL makes the upload a no-op.
func (a *DeliverActivities) Deliver(ctx context.Context, input DeliverInput) error {
	client := webhook.New(input.WebhookURL)

	// Emit liveness heartbeats during the multipart upload so a hard data-worker
	// death mid-Deliver is detected within activityHeartbeatTimeout (2 min) rather
	// than only after the 30-minute deliverTimeout. The HeartbeatTimeout on the
	// activity options requires these heartbeats — without them Temporal would fail
	// the (otherwise non-heartbeating) activity spuriously.
	if err := withActivityHeartbeat(ctx, func() error {
		return client.SendFile(ctx, input.ReportPath)
	}); err != nil {
		return classifyDeliverError(fmt.Errorf("deliver report %q: %w", input.ReportPath, err))
	}

	return nil
}

// classifyDeliverError maps a Deliver upload failure to its Temporal retry semantics,
// mirroring verify.go's classifyVerifyError. A deterministic webhook rejection — a
// permanent 4xx surfaced as a non-retryable *webhook.StatusError — is wrapped as a
// non-retryable ApplicationError so the run fails on the first attempt and the SPEC
// §11 failure alert fires, instead of exhausting deliverMaxAttempts on an upload that
// can never succeed. Every other failure (a retryable 429/5xx status, or a transport
// error) is returned unwrapped so it stays retryable and the bounded RetryPolicy
// governs (matches the report.go / verify.go convention).
func classifyDeliverError(err error) error {
	var statusErr *webhook.StatusError
	if errors.As(err, &statusErr) && !statusErr.Retryable() {
		return temporal.NewNonRetryableApplicationError(err.Error(), deliverWebhookRejectedErrorType, err)
	}

	return err
}

// deliverPhase orchestrates the Deliver phase (SPEC §4.3 phase 11): it runs the
// data-side Deliver activity with the webhook target and the report path the Report
// phase recorded in runState.
func deliverPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: deliverTimeout,
		HeartbeatTimeout:    activityHeartbeatTimeout,
		// Bound retries so a webhook that never recovers terminates the run (and
		// fires the SPEC §11 failure alert) instead of looping forever under the
		// server-default unlimited policy — Deliver is the final phase, so an
		// unbounded retry wedges the run silently. A deterministic 4xx short-circuits
		// this budget via classifyDeliverError (non-retryable, fails on attempt 1); a
		// transient 429/5xx/transport blip gets deliverMaxAttempts to recover.
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: deliverMaxAttempts},
	})

	var activities *DeliverActivities

	input := DeliverInput{
		WebhookURL: cfg.Delivery.WebhookURL,
		ReportPath: state.reportPath,
	}

	return workflow.ExecuteActivity(dataCtx, activities.Deliver, input).Get(dataCtx, nil)
}
