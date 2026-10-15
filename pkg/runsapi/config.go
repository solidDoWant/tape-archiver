// GET /api/runs/{runID}/config (issue #273): the submitted run config,
// recovered from WorkflowExecutionStarted's Input, never persisted separately
// (SPEC §4.2).
package runsapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// redactedSecret replaces a secret field's value in RunConfigResponse.
const redactedSecret = "***redacted***"

// RunConfigResponse is the GET /api/runs/{runID}/config response body.
type RunConfigResponse struct {
	RunID string `json:"runId"`
	// DryRun comes from the run's memo: the Config itself does not record it,
	// and a restart must not silently turn a dry-run into a production run.
	DryRun bool          `json:"dryRun"`
	Config config.Config `json:"config"`
}

func (h *handler) getRunConfig(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	runID := r.PathValue("runID")
	if runID == "" {
		writeError(w, http.StatusBadRequest, errors.New("runID is required"))

		return
	}

	history, err := fetchRunHistory(ctx, h.temporalClient, runID)
	if err != nil {
		writeHistoryError(ctx, w, h.temporalClient, runID, err)

		return
	}

	if history.StartInput == nil {
		// Only a foreign/stub workflow sharing the WorkflowID lacks a start input.
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Errorf("run %q has no recorded start input to decode a config from", runID))

		return
	}

	var cfg config.Config
	if err := decodePayloads(history.StartInput, &cfg); err != nil {
		writeError(w, http.StatusUnprocessableEntity, fmt.Errorf("decode run %q submitted config: %w", runID, err))

		return
	}

	redactConfigSecrets(&cfg)

	writeJSON(w, http.StatusOK, RunConfigResponse{
		RunID:  runID,
		DryRun: dryRunFromMemo(history.StartMemo),
		Config: cfg,
	})
}

// redactConfigSecrets strips the only two credential-bearing config fields:
// the age private identity and the Discord webhook URL (its path embeds the
// auth token). Every other field is non-secret and returned unmodified; this
// narrowing of issue #273's AC4 is recorded on that issue.
func redactConfigSecrets(cfg *config.Config) {
	if cfg.Encryption.Identity != "" {
		cfg.Encryption.Identity = redactedSecret
	}

	if cfg.Delivery.WebhookURL != "" {
		cfg.Delivery.WebhookURL = redactedSecret
	}
}
