package runsapi

import "net/http"

// uiConfigResponse is GET /api/config/ui's body: deploy-time, non-sensitive
// config the SPA fetches once. Library and Delivery are deploy-owned values the
// guided form shows read-only (issue #304); they still land in the submitted run
// config, which stays the single source of truth (SPEC §4.2).
type uiConfigResponse struct {
	// TemporalUIBaseURL is "" when no Temporal Web UI is configured.
	TemporalUIBaseURL string `json:"temporalUiBaseUrl"`
	// TemporalNamespace is the {namespace} segment of a Temporal Web UI deep-link.
	TemporalNamespace string `json:"temporalNamespace"`
	// Library fields are empty when unconfigured, leaving internal/config's
	// validation to report it rather than the SPA guessing a default.
	Library uiLibraryConfig `json:"library"`
	// Delivery holds the webhook flag (issue #304) and burner drives (issue #317).
	Delivery uiDeliveryConfig `json:"delivery"`
	// Presets are the deployment's valid run presets (issue #417); [] when none.
	Presets []uiPreset `json:"presets"`
}

// uiLibraryConfig holds the deploy-owned devices (issue #304) and physical
// topology (issue #305) the slot-grid picker is drawn from.
type uiLibraryConfig struct {
	Changer string   `json:"changer"`
	Drives  []string `json:"drives"`
	// SlotCount is 0 when no topology was declared.
	SlotCount int `json:"slotCount"`
	// CleaningSlots and IOStationSlots are never selectable as write targets.
	CleaningSlots  []int `json:"cleaningSlots"`
	IOStationSlots []int `json:"ioStationSlots"`
}

// uiDeliveryConfig reports only whether a webhook is configured, never the URL:
// it embeds a posting token, and cmd/web re-applies it server-side anyway.
type uiDeliveryConfig struct {
	WebhookConfigured bool     `json:"webhookConfigured"`
	OpticalBurnDrives []string `json:"opticalBurnDrives"`
}

func (h *handler) getUIConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, uiConfigResponse{
		TemporalUIBaseURL: h.temporalUIBaseURL,
		TemporalNamespace: h.temporalNamespace,
		Library: uiLibraryConfig{
			Changer: h.deployChanger,
			// Normalize nil to [] so the SPA gets an array, not null.
			Drives:    append([]string{}, h.deployDrives...),
			SlotCount: h.deploySlotCount,

			CleaningSlots:  append([]int{}, h.deployCleaningSlots...),
			IOStationSlots: append([]int{}, h.deployIOStationSlots...),
		},
		Delivery: uiDeliveryConfig{
			// Never the URL itself: it is a credential.
			WebhookConfigured: h.deployWebhookURL != "",
			// Same [] normalization as Drives.
			OpticalBurnDrives: append([]string{}, h.deployOpticalBurnerDrives...),
		},
		Presets: append([]uiPreset{}, h.presets...),
	})
}
