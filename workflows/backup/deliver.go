package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/webhook"
)

// The Deliver phase (SPEC §4.3 phase 11) uploads the PDF report to the run's
// Discord success webhook. It runs on the data worker so the report never crosses
// a Temporal payload; an empty webhook URL makes it a no-op.

const deliverTimeout = 30 * time.Minute

// deliverMaxAttempts bounds transient retries: Deliver is the final phase, so an
// unbounded policy would wedge the run and the failure alert would never fire.
const deliverMaxAttempts = 5

// deliverWebhookRejectedErrorType marks a permanent 4xx webhook rejection.
const deliverWebhookRejectedErrorType = "deliver-webhook-rejected"

// DeliverActivities hosts the data-side Deliver activity.
type DeliverActivities struct{}

// newDeliverActivities returns the data-side Deliver activity.
func newDeliverActivities() *DeliverActivities { return &DeliverActivities{} }

// DeliverInput is the payload for the Deliver activity. An empty WebhookURL
// disables delivery.
type DeliverInput struct {
	WebhookURL string
	ReportPath string
	// Summary is the message posted alongside the report.
	Summary string
}

// DeliverResult identifies the posted Discord message so the run overview can
// deep-link to it (issue #306). It lives only in workflow history (SPEC §4.2) and
// is zero-valued when delivery was skipped or the response was unparseable.
type DeliverResult struct {
	// GuildID comes from a best-effort webhook lookup; the others from the
	// ?wait=true post response.
	GuildID   string
	ChannelID string
	MessageID string
}

// Deliver uploads the report to the configured Discord webhook. A permanent 4xx
// is returned non-retryable so the run fails fast.
func (a *DeliverActivities) Deliver(ctx context.Context, input DeliverInput) (*DeliverResult, error) {
	client := webhook.New(input.WebhookURL)

	var posted *webhook.PostedMessage

	if err := withActivityHeartbeat(ctx, func() error {
		var sendErr error

		posted, sendErr = client.SendFile(ctx, input.ReportPath, input.Summary)

		return sendErr
	}); err != nil {
		return nil, classifyDeliverError(fmt.Errorf("deliver report %q: %w", input.ReportPath, err))
	}

	// From here on everything is best-effort deep-link metadata (issue #306) and
	// must never fail a delivered run.
	result := &DeliverResult{}

	if posted == nil {
		if input.WebhookURL == "" {
			slog.InfoContext(ctx, "deliver: no success webhook configured; report delivery skipped")
		}

		return result, nil
	}

	result.ChannelID = posted.ChannelID
	result.MessageID = posted.ID

	// The post response lacks the guild; a failed lookup only omits the link.
	if guildID, err := client.FetchWebhookGuild(ctx); err != nil {
		slog.WarnContext(ctx, "deliver: could not resolve webhook guild; report deep-link omitted", "error", err)
	} else {
		result.GuildID = guildID
	}

	slog.InfoContext(ctx, "deliver: uploaded the report to the Discord success webhook",
		"channel", result.ChannelID, "message", result.MessageID)

	return result, nil
}

// classifyDeliverError makes a permanent 4xx non-retryable; everything else stays
// retryable under the bounded RetryPolicy.
func classifyDeliverError(err error) error {
	var statusErr *webhook.StatusError
	if errors.As(err, &statusErr) && !statusErr.Retryable() {
		return temporal.NewNonRetryableApplicationError(err.Error(), deliverWebhookRejectedErrorType, err)
	}

	return err
}

// deliverySummary composes the short message posted with the report: the run,
// what was archived, and onto which tapes.
func deliverySummary(runID string, state *runState, dryRun bool) string {
	var (
		archives int64
		bytes    int64
	)

	for _, staged := range state.staged {
		archives++
		bytes += staged.SizeBytes
	}

	barcodes := make([]string, 0, len(state.written))
	for _, written := range state.written {
		barcodes = append(barcodes, string(written.Barcode))
	}

	header := "📼 **Backup complete**"
	if dryRun {
		header = "🧪 **Dry run complete** — a rehearsal against the virtual library; no real tapes were written"
	}

	lines := []string{
		header,
		fmt.Sprintf("**Run:** `%s`", runID),
		fmt.Sprintf("**Archived:** %d %s (%s)", archives, plural(archives, "archive"), humanBytes(bytes)),
	}

	tapesLabel := "Tapes"
	if dryRun {
		tapesLabel = "Virtual tapes"
	}

	if len(barcodes) > 0 {
		lines = append(lines, fmt.Sprintf("**%s:** %s", tapesLabel, strings.Join(barcodes, ", ")))
	}

	if dryRun {
		lines = append(lines, "", "The attached report is from a rehearsal — do not file it with any tapes.")
	} else {
		lines = append(lines, "",
			"The attached report is this run's recovery index — print it and store it with the tapes.")
	}

	return strings.Join(lines, "\n")
}

// plural renders a count's noun, so a one-archive run does not read "1 archives".
func plural(count int64, noun string) string {
	if count == 1 {
		return noun
	}

	return noun + "s"
}

// humanBytes renders a byte count in decimal units, e.g. "13.9 GB".
func humanBytes(bytes int64) string {
	const unit = 1000

	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	value := float64(bytes)
	units := []string{"kB", "MB", "GB", "TB", "PB"}

	var suffix string

	for _, u := range units {
		value /= unit
		suffix = u

		if value < unit {
			break
		}
	}

	return fmt.Sprintf("%.1f %s", value, suffix)
}

// deliverPhase runs the data-side Deliver activity.
func deliverPhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: deliverTimeout,
		HeartbeatTimeout:    activityHeartbeatTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: deliverMaxAttempts},
	})

	var activities *DeliverActivities

	input := DeliverInput{
		WebhookURL: cfg.Delivery.WebhookURL,
		ReportPath: state.reportPath,
		Summary:    deliverySummary(workflow.GetInfo(ctx).WorkflowExecution.RunID, state, isDryRun(ctx)),
	}

	// The result lives only in history; pkg/runsapi reads it from there (#306).
	return workflow.ExecuteActivity(dataCtx, activities.Deliver, input).Get(dataCtx, nil)
}
