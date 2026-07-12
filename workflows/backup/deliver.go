package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// DeliverResult records the identity of the Discord message the report was posted
// to, so the run's overview can deep-link to it
// (https://discord.com/channels/{guild}/{channel}/{message}, issue #306). It is
// persisted in Temporal workflow history as the activity's result and later
// reconstructed by pkg/runsapi — no external catalog, honoring SPEC §4.2. Every
// field is best-effort: delivery with no webhook configured, or a Discord response
// the client could not parse, yields a zero-value result (an empty DeliverResult),
// which the runs API renders as "no link". A failed delivery produces no completed
// Deliver result at all.
type DeliverResult struct {
	// GuildID, ChannelID and MessageID are the three path segments of the
	// jump-to-message link. GuildID comes from a best-effort webhook lookup
	// (webhook.FetchWebhookGuild); the other two from the ?wait=true post response.
	GuildID   string
	ChannelID string
	MessageID string
}

// Deliver uploads the report to the configured Discord webhook (SPEC §4.3 phase 11,
// §11). A transient failure (a 429/5xx response or a transport error) fails the
// activity and Temporal retries it up to deliverMaxAttempts; a deterministic
// rejection (a permanent 4xx — a deleted/rotated webhook or an oversize report) is
// returned non-retryable so the run fails fast and the failure alert fires. An empty
// webhook URL makes the upload a no-op.
func (a *DeliverActivities) Deliver(ctx context.Context, input DeliverInput) (*DeliverResult, error) {
	client := webhook.New(input.WebhookURL)

	// Emit liveness heartbeats during the multipart upload so a hard data-worker
	// death mid-Deliver is detected within activityHeartbeatTimeout (2 min) rather
	// than only after the 30-minute deliverTimeout. The HeartbeatTimeout on the
	// activity options requires these heartbeats — without them Temporal would fail
	// the (otherwise non-heartbeating) activity spuriously.
	var posted *webhook.PostedMessage

	if err := withActivityHeartbeat(ctx, func() error {
		var sendErr error

		posted, sendErr = client.SendFile(ctx, input.ReportPath)

		return sendErr
	}); err != nil {
		return nil, classifyDeliverError(fmt.Errorf("deliver report %q: %w", input.ReportPath, err))
	}

	// Delivery succeeded. Everything from here is best-effort deep-link metadata
	// (issue #306): it must never turn a delivered report into a failed run. A
	// no-op webhook (empty URL) or an unparseable response leaves posted nil, so
	// the result stays zero-valued and the runs API shows no link.
	result := &DeliverResult{}
	if posted == nil {
		return result, nil
	}

	result.ChannelID = posted.ChannelID
	result.MessageID = posted.ID

	// The ?wait=true post response gives the channel and message but not the guild
	// a jump-to-message URL also needs. Fetch it from the webhook object, tolerating
	// any error — a missing guild only omits the link (SPEC §4.2: no external
	// catalog, the run's own history carries the identity).
	if guildID, err := client.FetchWebhookGuild(ctx); err != nil {
		slog.WarnContext(ctx, "deliver: could not resolve webhook guild; report deep-link omitted", "error", err)
	} else {
		result.GuildID = guildID
	}

	return result, nil
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

	// The result (the posted message's identity) is captured in workflow history
	// as the activity's completion payload; pkg/runsapi reconstructs the run
	// overview's Discord deep-link from it (issue #306). The workflow keeps no copy
	// — history is the sole source (SPEC §4.2).
	return workflow.ExecuteActivity(dataCtx, activities.Deliver, input).Get(dataCtx, nil)
}
