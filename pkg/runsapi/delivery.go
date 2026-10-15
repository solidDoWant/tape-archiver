// GET /api/runs/{runID}/delivery (issue #306): the Discord deep-link for the
// run's report, reconstructed from workflow history, never persisted state (SPEC §4.2).
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
	// MessageURL is the Discord jump-to-message link, or "" when no fully
	// identified report was delivered.
	MessageURL string `json:"messageUrl"`
}

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

// deriveDeliveryMessageURL builds the link from the completed Deliver result.
// At most one Deliver is scheduled per run, so the first completed one decides.
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

// discordMessageURL returns "" if any segment is absent. The IDs come from
// Discord's own API responses, not user input.
func discordMessageURL(guildID, channelID, messageID string) string {
	if guildID == "" || channelID == "" || messageID == "" {
		return ""
	}

	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, channelID, messageID)
}
