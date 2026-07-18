package runsapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetUIConfig checks GET /api/config/ui reports exactly the Temporal Web UI
// base URL and namespace supplied via WithTemporalUI and the deploy-owned
// library devices/webhook supplied via WithDeployConfig, so the SPA can build a
// correct workflow deep-link (and omit it when no UI is configured) and source
// the guided form's device/webhook values from deploy config (issue #304).
func TestGetUIConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                      string
		opts                      []Option
		expectedBaseURL           string
		expectedNamespace         string
		expectedChanger           string
		expectedDrives            []string
		expectedWebhookConfigured bool
		expectedOpticalBurnDrive  []string
		expectedSlotCount         int
		expectedCleaningSlots     []int
		expectedIOStationSlots    []int
	}{
		{
			name: "configured",
			opts: []Option{
				WithTemporalUI("https://temporal.example.com", "prod"),
				WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, "https://discord.example/webhook"),
				WithOpticalBurnerDrives([]string{"/dev/sr0", "/dev/sr1"}),
				WithLibraryTopology(47, []int{45}, []int{46, 47}),
			},
			expectedBaseURL:           "https://temporal.example.com",
			expectedNamespace:         "prod",
			expectedChanger:           "/dev/sch0",
			expectedDrives:            []string{"/dev/nst0", "/dev/nst1"},
			expectedWebhookConfigured: true,
			expectedOpticalBurnDrive:  []string{"/dev/sr0", "/dev/sr1"},
			expectedSlotCount:         47,
			expectedCleaningSlots:     []int{45},
			expectedIOStationSlots:    []int{46, 47},
		},
		{
			name:              "unconfigured reports empty so the SPA omits the link and surfaces validation",
			opts:              nil,
			expectedBaseURL:   "",
			expectedNamespace: "",
			expectedChanger:   "",
			// An unconfigured drive set is normalized to a non-nil empty
			// slice so the JSON is [] (an array the SPA can map over), never
			// null.
			expectedDrives:            []string{},
			expectedWebhookConfigured: false,
			// Same [] (never null) normalization for the burner drives.
			expectedOpticalBurnDrive: []string{},
			// An undeclared topology reports a 0 slot count and empty (not
			// null) reserved-slot arrays, so the SPA's picker shows the
			// "not configured" state.
			expectedSlotCount:      0,
			expectedCleaningSlots:  []int{},
			expectedIOStationSlots: []int{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			mux := newMux(newHandler(&fakeTemporalClient{}, func(string) string { return "" }, test.opts...))

			request := httptest.NewRequest(http.MethodGet, "/api/config/ui", nil)
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)

			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.Equal(t, "application/json", recorder.Header().Get("Content-Type"))

			var decoded uiConfigResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &decoded))
			assert.Equal(t, test.expectedBaseURL, decoded.TemporalUIBaseURL)
			assert.Equal(t, test.expectedNamespace, decoded.TemporalNamespace)
			assert.Equal(t, test.expectedChanger, decoded.Library.Changer)
			assert.Equal(t, test.expectedDrives, decoded.Library.Drives)
			assert.Equal(t, test.expectedWebhookConfigured, decoded.Delivery.WebhookConfigured)
			assert.Equal(t, test.expectedOpticalBurnDrive, decoded.Delivery.OpticalBurnDrives)
			assert.Equal(t, test.expectedSlotCount, decoded.Library.SlotCount)
			assert.Equal(t, test.expectedCleaningSlots, decoded.Library.CleaningSlots)
			assert.Equal(t, test.expectedIOStationSlots, decoded.Library.IOStationSlots)
		})
	}
}
