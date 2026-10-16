package runsapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fetchUIPresets returns the presets GET /api/config/ui offers.
func fetchUIPresets(t *testing.T, opts ...Option) []uiPreset {
	t.Helper()

	mux := newMux(newHandler(&fakeTemporalClient{}, func(string) string { return "" }, opts...))

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/config/ui", nil))
	require.Equal(t, http.StatusOK, recorder.Code)

	var decoded uiConfigResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &decoded))

	return decoded.Presets
}

func presetNames(presets []uiPreset) []string {
	names := make([]string, 0, len(presets))
	for _, preset := range presets {
		names = append(names, preset.Name)
	}

	return names
}

func TestGetUIConfigPresets(t *testing.T) {
	t.Parallel()

	deploy := []Option{
		WithDeployConfig("/dev/sch0", []string{"/dev/nst0", "/dev/nst1"}, ""),
		WithLibraryTopology(10, []int{9}, []int{10}),
	}

	tests := []struct {
		name          string
		opts          []Option
		presets       []Preset
		expectedNames []string
	}{
		{
			name:          "no presets configured offers an empty list",
			opts:          deploy,
			presets:       nil,
			expectedNames: []string{},
		},
		{
			name: "valid partial presets are offered in order",
			opts: deploy,
			presets: []Preset{
				{Name: "nightly", Description: "Nightly app snapshots", Config: json.RawMessage(`{"sources":[{"k8s":{"apiVersion":"snapshot.storage.k8s.io/v1","kind":"VolumeSnapshot","labelSelector":"backup=nightly","select":"newestPerPVC"}},{"zfsPath":{"name":"bulk-pool-01/archive","snapshot":"newest","prefix":"daily-"}}]}`)},
				{Name: "archive", Config: json.RawMessage(`{"sources":[{"zfsPath":{"name":"bulk-pool-01/archive"}}],"copies":2,"library":{"blankSlots":[1,2,3,4]},"redundancy":{"fillToCapacity":{"floor":5}},"encryption":{"recipients":["age1abc"]},"delivery":{"opticalBurn":{"copies":1}}}`)},
			},
			expectedNames: []string{"nightly", "archive"},
		},
		{
			name: "invalid presets are dropped while valid ones stay",
			opts: deploy,
			presets: []Preset{
				{Name: "ok", Config: json.RawMessage(`{"copies":1}`)},
				{Name: "", Config: json.RawMessage(`{}`)},
				{Name: "ok", Config: json.RawMessage(`{}`)},
				{Name: "not-object", Config: json.RawMessage(`[1]`)},
				{Name: "unknown-field", Config: json.RawMessage(`{"copiez":1}`)},
				{Name: "wrong-type", Config: json.RawMessage(`{"copies":"two"}`)},
				{Name: "zero-copies", Config: json.RawMessage(`{"copies":0}`)},
				{Name: "zero-copies-other-case", Config: json.RawMessage(`{"Copies":0}`)},
				{Name: "empty-sources-other-case", Config: json.RawMessage(`{"Sources":[]}`)},
				{Name: "empty-sources", Config: json.RawMessage(`{"sources":[]}`)},
				{Name: "bad-source", Config: json.RawMessage(`{"sources":[{"zfsPath":{"name":""}}]}`)},
				{Name: "prefix-without-snapshot", Config: json.RawMessage(`{"sources":[{"zfsPath":{"name":"bulk-pool-01/archive","prefix":"daily-"}}]}`)},
				{Name: "select-without-selector", Config: json.RawMessage(`{"sources":[{"k8s":{"apiVersion":"snapshot.storage.k8s.io/v1","kind":"VolumeSnapshot","namespace":"apps","name":"db","select":"newestPerPVC"}}]}`)},
				{Name: "slots-not-multiple", Config: json.RawMessage(`{"copies":2,"library":{"blankSlots":[1,2,3]}}`)},
				{Name: "slot-outside-library", Config: json.RawMessage(`{"library":{"blankSlots":[11]}}`)},
				{Name: "reserved-slot", Config: json.RawMessage(`{"library":{"blankSlots":[9]}}`)},
				{Name: "both-redundancy", Config: json.RawMessage(`{"redundancy":{"targetPercentage":10,"fillToCapacity":{"floor":5}}}`)},
				{Name: "identity", Config: json.RawMessage(`{"encryption":{"identity":"AGE-SECRET-KEY-1X"}}`)},
				{Name: "webhook", Config: json.RawMessage(`{"delivery":{"webhookUrl":"https://discord.example/x"}}`)},
				{Name: "decode-error", decodeErr: assert.AnError},
			},
			expectedNames: []string{"ok"},
		},
		{
			name: "deploy-owned devices override preset values before validation",
			opts: deploy,
			presets: []Preset{
				{Name: "duplicate-drives-overridden", Config: json.RawMessage(`{"library":{"drives":["/dev/x","/dev/x"]}}`)},
			},
			expectedNames: []string{"duplicate-drives-overridden"},
		},
		{
			name: "preset devices are validated when the deployment owns none",
			opts: nil,
			presets: []Preset{
				{Name: "duplicate-drives", Config: json.RawMessage(`{"library":{"drives":["/dev/x","/dev/x"]}}`)},
				{Name: "any-slot", Config: json.RawMessage(`{"library":{"blankSlots":[99]}}`)},
			},
			expectedNames: []string{"any-slot"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			opts := append(append([]Option{}, test.opts...), WithPresets(test.presets))
			assert.Equal(t, test.expectedNames, presetNames(fetchUIPresets(t, opts...)))
		})
	}
}

func TestGetUIConfigPresetServesPresetAsWritten(t *testing.T) {
	t.Parallel()

	config := `{"sources":[{"zfsPath":{"name":"bulk-pool-01/archive"}}],"library":{"tapeCapacityBytes":18000000000000}}`

	presets := fetchUIPresets(t,
		WithDeployConfig("/dev/sch0", []string{"/dev/nst0"}, ""),
		WithPresets([]Preset{{Name: "archive", Description: "Host datasets", Config: json.RawMessage(config)}}),
	)
	require.Len(t, presets, 1)

	assert.Equal(t, "archive", presets[0].Name)
	assert.Equal(t, "Host datasets", presets[0].Description)

	// Neither placeholders nor deploy fields leak in, and large integers stay exact.
	served, err := json.Marshal(presets[0].Config)
	require.NoError(t, err)
	assert.JSONEq(t, config, string(served))
}

// Not parallel: it swaps the global slog default.
func TestInvalidPresetIsLoggedWithNameAndError(t *testing.T) {
	var logs bytes.Buffer

	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	presets := fetchUIPresets(t, WithPresets([]Preset{{Name: "broken-preset", Config: json.RawMessage(`{"copies":0}`)}}))

	assert.Empty(t, presets)
	assert.Contains(t, logs.String(), "preset=broken-preset")
	assert.Contains(t, logs.String(), "copies: must be at least 1")
}

func TestLoadPresetsFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		content       *string
		expected      []Preset
		expectedError require.ErrorAssertionFunc
		// expectedDecodeFailures names entries kept with a per-entry decode error.
		expectedDecodeFailures []string
	}{
		{
			name:          "missing file is an error",
			content:       nil,
			expectedError: require.Error,
		},
		{
			name:          "non-array document is an error",
			content:       new(`{"name":"x"}`),
			expectedError: require.Error,
		},
		{
			name:     "valid presets decode",
			content:  new(`[{"name":"nightly","description":"d","config":{"copies":2}}]`),
			expected: []Preset{{Name: "nightly", Description: "d", Config: json.RawMessage(`{"copies":2}`)}},
		},
		{
			name:                   "malformed entry is kept for validation by name",
			content:                new(`[{"name":"typo","descripton":"d","config":{}}]`),
			expectedDecodeFailures: []string{"typo"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.expectedError == nil {
				test.expectedError = require.NoError
			}

			path := filepath.Join(t.TempDir(), "presets.json")
			if test.content != nil {
				require.NoError(t, os.WriteFile(path, []byte(*test.content), 0o600))
			}

			presets, err := LoadPresetsFile(path)
			test.expectedError(t, err)

			if test.expectedDecodeFailures != nil {
				var failed []string

				for _, preset := range presets {
					if preset.decodeErr != nil {
						failed = append(failed, preset.Name)
					}
				}

				assert.Equal(t, test.expectedDecodeFailures, failed)

				return
			}

			assert.Equal(t, test.expected, presets)
		})
	}
}

func TestLoadPresetsFileEmptyPathYieldsNone(t *testing.T) {
	t.Parallel()

	presets, err := LoadPresetsFile("")
	require.NoError(t, err)
	assert.Empty(t, presets)
}
