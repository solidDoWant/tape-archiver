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
// base URL and namespace supplied via WithTemporalUI, so the SPA can build a
// correct workflow deep-link (and omit it when no UI is configured).
func TestGetUIConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		opts              []Option
		expectedBaseURL   string
		expectedNamespace string
	}{
		{
			name:              "configured",
			opts:              []Option{WithTemporalUI("https://temporal.example.com", "prod")},
			expectedBaseURL:   "https://temporal.example.com",
			expectedNamespace: "prod",
		},
		{
			name:              "unconfigured reports empty so the SPA omits the link",
			opts:              nil,
			expectedBaseURL:   "",
			expectedNamespace: "",
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
		})
	}
}
