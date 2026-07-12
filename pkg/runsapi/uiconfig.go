package runsapi

import "net/http"

// uiConfigResponse is GET /api/config/ui's JSON body: server-provided
// deploy-time config the SPA needs to build outbound links (currently just the
// run overview's Temporal Web UI deep-link). It carries no per-run or sensitive
// data — the whole /api surface is already session-gated (pkg/webauth), and
// these are deploy-time values (a UI base URL and the Temporal namespace) — so
// a single cached fetch by the SPA is all it needs.
type uiConfigResponse struct {
	// TemporalUIBaseURL is the browsable Temporal Web UI base URL (cmd/web's
	// TEMPORAL_UI_URL), or "" when no UI is configured — the SPA omits the
	// Temporal-workflow link in that case.
	TemporalUIBaseURL string `json:"temporalUiBaseUrl"`
	// TemporalNamespace is the namespace runs execute in, i.e. the
	// {namespace} path segment of a Temporal Web UI workflow deep-link.
	TemporalNamespace string `json:"temporalNamespace"`
}

// getUIConfig implements GET /api/config/ui — see uiConfigResponse. It reports
// the values supplied via WithTemporalUI, both empty when the host did not
// configure a Temporal UI URL.
func (h *handler) getUIConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, uiConfigResponse{
		TemporalUIBaseURL: h.temporalUIBaseURL,
		TemporalNamespace: h.temporalNamespace,
	})
}
