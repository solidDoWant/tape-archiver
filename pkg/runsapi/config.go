// This file implements GET /api/runs/{runID}/config (issue #273): the exact
// run configuration originally submitted for a run, recovered from
// WorkflowExecutionStarted's own recorded Input — runsubmit.Submit's
// ExecuteWorkflow(ctx, opts, backup.WorkflowType, cfg) call means that event
// carries the very *config.Config runsapi.submitRun (or `tapectl run`) passed
// in, byte-for-byte as Temporal received it. Never persisted separately
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
	RunID  string        `json:"runId"`
	Config config.Config `json:"config"`
}

// getRunConfig implements GET /api/runs/{runID}/config.
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
		// Every real execution of the Backup workflow type always has a
		// WorkflowExecutionStarted event with an Input payload — this only
		// fires against a foreign/stub workflow sharing the fixed WorkflowID
		// (issue #273's warning about non-Backup histories, e.g. this
		// package's own integration tests' stub workflows), which this
		// endpoint cannot meaningfully answer for.
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

	writeJSON(w, http.StatusOK, RunConfigResponse{RunID: runID, Config: cfg})
}

// redactConfigSecrets strips credential-bearing fields before a submitted
// config leaves the server. This endpoint exists to reproduce the config for
// *display* (Sources list, tape-capacity/redundancy/delivery summaries,
// DESIGN_ANALYSIS.md §5's Tapes-page CONTENTS column), none of which needs a
// credential. Two fields qualify — the result of a deliberate sweep of every
// field internal/config.Config carries (sources/labels/k8s refs, device
// paths, slots, capacities, timeouts, percentages, and OpticalBurn's
// drives/copies/flags are all non-secret operational values):
//
//   - Encryption.Identity: the age *private* key that decrypts every archive
//     the run protects, escrowed into the report and recovery ISO
//     specifically so a human holding *those* physical/printed artifacts can
//     recover data (SPEC §7) — not something a browser session should read
//     back over a GET. Encryption.Recipients (public keys) are left
//     untouched: no such risk, and useful to display as-is.
//   - Delivery.WebhookURL: a Discord webhook URL embeds its auth token in
//     the path — the URL alone lets anyone post to the channel — so it is a
//     credential, not an address.
//
// This is a deliberate scope narrowing of AC4 ("the exact run configuration
// originally submitted... is returned"), documented as a decision on issue
// #273 rather than silently applied: every field is returned unmodified
// except these two secrets.
func redactConfigSecrets(cfg *config.Config) {
	if cfg.Encryption.Identity != "" {
		cfg.Encryption.Identity = redactedSecret
	}

	if cfg.Delivery.WebhookURL != "" {
		cfg.Delivery.WebhookURL = redactedSecret
	}
}
