package runsapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			// Unconfigured sets must serialize as [], never null.
			expectedDrives:            []string{},
			expectedWebhookConfigured: false,

			expectedOpticalBurnDrive: []string{},

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
