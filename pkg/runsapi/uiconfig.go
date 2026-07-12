package runsapi

import "net/http"

// uiConfigResponse is GET /api/config/ui's JSON body: server-provided
// deploy-time config the SPA needs. It carries no per-run or sensitive data —
// the whole /api surface is already session-gated (pkg/webauth), and these are
// deploy-time values (a UI base URL, the Temporal namespace, and the
// deployment's fixed hardware/delivery targets) — so a single cached fetch by
// the SPA is all it needs.
//
// The Library and Delivery values are the deploy-owned config the guided config
// form's Library and Delivery sections source read-only rather than exposing as
// per-run free-text inputs (issue #304): a changer/drive device path or a
// Discord webhook URL is a property of the deployment/host, not a per-run
// choice, so the operator should not re-type (or mis-type) it on every
// submission. They still end up in the submitted run config — the SPA fills
// them in from here — so the run config stays the single source of truth
// (SPEC §4.2); this only moves *where the operator supplies them*. The library
// values are nested under Library so the companion library-topology config
// (issue #305) can extend the same object without a second endpoint.
type uiConfigResponse struct {
	// TemporalUIBaseURL is the browsable Temporal Web UI base URL (cmd/web's
	// TEMPORAL_UI_URL), or "" when no UI is configured — the SPA omits the
	// Temporal-workflow link in that case.
	TemporalUIBaseURL string `json:"temporalUiBaseUrl"`
	// TemporalNamespace is the namespace runs execute in, i.e. the
	// {namespace} path segment of a Temporal Web UI workflow deep-link.
	TemporalNamespace string `json:"temporalNamespace"`
	// Library carries the deployment's fixed library device targets (issue
	// #304). Empty fields when the host did not configure them, in which case
	// the guided form's Review step surfaces internal/config's own
	// "changer must not be empty" / "at least one drive is required"
	// validation rather than the SPA guessing a default.
	Library uiLibraryConfig `json:"library"`
	// Delivery carries the deployment's fixed delivery targets (issue #304) —
	// currently just the Discord webhook URL, empty when unconfigured.
	Delivery uiDeliveryConfig `json:"delivery"`
}

// uiLibraryConfig is uiConfigResponse's deploy-owned library section: the
// changer and drive device paths the guided form fills in from deploy config.
type uiLibraryConfig struct {
	Changer string   `json:"changer"`
	Drives  []string `json:"drives"`
}

// uiDeliveryConfig is uiConfigResponse's deploy-owned delivery section.
type uiDeliveryConfig struct {
	WebhookURL string `json:"webhookUrl"`
}

// getUIConfig implements GET /api/config/ui — see uiConfigResponse. It reports
// the values supplied via WithTemporalUI and WithDeployConfig, all empty when
// the host did not configure them.
func (h *handler) getUIConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, uiConfigResponse{
		TemporalUIBaseURL: h.temporalUIBaseURL,
		TemporalNamespace: h.temporalNamespace,
		Library: uiLibraryConfig{
			Changer: h.deployChanger,
			// A nil slice marshals to JSON null; the SPA expects an array, so
			// normalize an unconfigured drive set to [].
			Drives: append([]string{}, h.deployDrives...),
		},
		Delivery: uiDeliveryConfig{
			WebhookURL: h.deployWebhookURL,
		},
	})
}
