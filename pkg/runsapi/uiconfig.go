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
	// Delivery carries the deployment's fixed delivery targets: the Discord
	// webhook URL (issue #304) and the optical burner device paths (issue #317),
	// each empty/[] when unconfigured.
	Delivery uiDeliveryConfig `json:"delivery"`
}

// uiLibraryConfig is uiConfigResponse's deploy-owned library section: the
// changer and drive device paths the guided form fills in from deploy config
// (issue #304), plus the library's physical topology (issue #305) — the storage
// slot count and the cleaning / I/O-station slot numbers — from which the guided
// form renders a real slot-grid picker bounded to that library instead of a
// free-form list of slot numbers. The topology is deploy-owned for the same
// reason as the devices: it is a property of the physical library, not a per-run
// choice.
type uiLibraryConfig struct {
	Changer string   `json:"changer"`
	Drives  []string `json:"drives"`
	// SlotCount is the number of physical storage slots; the SPA numbers them
	// 1..SlotCount to draw the slot-grid picker. 0 when the deployment did not
	// declare a topology, in which case the form shows the picker as "not
	// configured" (see ConfigForm.tsx).
	SlotCount int `json:"slotCount"`
	// CleaningSlots and IOStationSlots are storage-slot numbers reserved for
	// cleaning cartridges and the I/O station (import/export / mail slot); the
	// picker renders them disabled so they can never be chosen as a blank
	// write-target slot. Each is [] when unconfigured, never null.
	CleaningSlots  []int `json:"cleaningSlots"`
	IOStationSlots []int `json:"ioStationSlots"`
}

// uiDeliveryConfig is uiConfigResponse's deploy-owned delivery section: the
// Discord webhook URL (issue #304) and the optical burner device paths (issue
// #317) the guided config form sources read-only. OpticalBurnDrives is [] (never
// null) when the deployment did not configure any, in which case a run that
// enables optical burn falls back to internal/config's own validation.
type uiDeliveryConfig struct {
	WebhookURL        string   `json:"webhookUrl"`
	OpticalBurnDrives []string `json:"opticalBurnDrives"`
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
			Drives:    append([]string{}, h.deployDrives...),
			SlotCount: h.deploySlotCount,
			// Same nil-slice normalization as Drives above: the SPA maps over
			// these, so an unconfigured set must be [] rather than null.
			CleaningSlots:  append([]int{}, h.deployCleaningSlots...),
			IOStationSlots: append([]int{}, h.deployIOStationSlots...),
		},
		Delivery: uiDeliveryConfig{
			WebhookURL: h.deployWebhookURL,
			// Same nil-slice normalization as Library.Drives: the SPA maps over
			// these, so an unconfigured set must be [] rather than null.
			OpticalBurnDrives: append([]string{}, h.deployOpticalBurnerDrives...),
		},
	})
}
