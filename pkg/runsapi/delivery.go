// This file implements GET /api/runs/{runID}/delivery (issue #306): the Discord
// jump-to-message deep-link for the run's posted PDF report, reconstructed from
// the Deliver activity's result recorded in the run's raw workflow history
// (history.go) — never from persisted state (SPEC §4.2). The run overview renders
// a "Discord report ↗" link beside the Temporal-workflow link when this endpoint
// returns a non-empty messageUrl.
package runsapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// RunDeliveryResponse is the GET /api/runs/{runID}/delivery response body.
type RunDeliveryResponse struct {
	RunID string `json:"runId"`
	// MessageURL is the Discord jump-to-message deep-link
	// (https://discord.com/channels/{guild}/{channel}/{message}) for this run's
	// posted report, or "" when the run delivered no report (delivery disabled or
	// not yet reached), the delivery failed (no completed Deliver activity), or the
	// message identity could not be fully reconstructed (e.g. an unresolved guild).
	// The SPA renders the "Discord report" link only when this is non-empty.
	MessageURL string `json:"messageUrl"`
}

// getRunDelivery implements GET /api/runs/{runID}/delivery.
func (h *handler) getRunDelivery(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("runID is required"))

		return
	}

	history, err := fetchRunHistory(ctx, h.temporalClient, runID)
	if err != nil {
		writeHistoryError(ctx, w, h.temporalClient, runID, err)

		return
	}

	writeJSON(w, http.StatusOK, RunDeliveryResponse{
		RunID:      runID,
		MessageURL: deriveDeliveryMessageURL(history.Activities),
	})
}

// deriveDeliveryMessageURL reconstructs the report's Discord deep-link from the
// completed Deliver activity's result (backup.DeliverResult, deliver.go). It
// returns "" when the run has no completed Deliver activity (delivery disabled,
// still in flight, or failed) or when the recorded identity is incomplete — a
// missing guild/channel/message segment cannot form a valid link. At most one
// Deliver activity is ever scheduled per run (it is the final phase), so the
// first completed one decides.
func deriveDeliveryMessageURL(activities []activityRecord) string {
	for _, record := range activities {
		if record.Name != "Deliver" || !record.Completed {
			continue
		}

		var result backup.DeliverResult
		if err := decodePayloads(record.Result, &result); err != nil {
			return ""
		}

		return discordMessageURL(result.GuildID, result.ChannelID, result.MessageID)
	}

	return ""
}

// discordMessageURL builds a Discord jump-to-message URL from its three snowflake
// segments, returning "" if any is absent (the link is only meaningful with all
// three). The IDs originate from Discord's own API responses (the webhook object
// and the ?wait=true execution response), so they are numeric snowflakes, not
// user input.
func discordMessageURL(guildID, channelID, messageID string) string {
	if guildID == "" || channelID == "" || messageID == "" {
		return ""
	}

	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, channelID, messageID)
}
