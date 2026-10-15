package runsapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerateAgeKeypairShape uses the real age-keygen binary.
func TestGenerateAgeKeypairShape(t *testing.T) {
	t.Parallel()

	mux := newMux(newHandler(&fakeTemporalClient{}, func(string) string { return "" }))

	recorder := postJSON(t, mux, "/api/age/keygen", nil, nil)
	assert.Equal(t, http.StatusOK, recorder.Code)

	assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	assert.Equal(t, "no-cache", recorder.Header().Get("Pragma"))

	var response AgeKeygenResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))

	assert.True(t, strings.HasPrefix(response.Recipient, "age1pq1"), "recipient must be post-quantum: %q", response.Recipient)
	assert.True(t, strings.HasPrefix(response.Identity, "AGE-SECRET-KEY-PQ-1"), "identity must be post-quantum: %q", response.Identity)
}

func TestGenerateAgeKeypairIsFreshEveryCall(t *testing.T) {
	t.Parallel()

	mux := newMux(newHandler(&fakeTemporalClient{}, func(string) string { return "" }))

	var first, second AgeKeygenResponse

	firstRecorder := postJSON(t, mux, "/api/age/keygen", nil, nil)
	require.NoError(t, json.Unmarshal(firstRecorder.Body.Bytes(), &first))

	secondRecorder := postJSON(t, mux, "/api/age/keygen", nil, nil)
	require.NoError(t, json.Unmarshal(secondRecorder.Body.Bytes(), &second))

	assert.NotEqual(t, first.Identity, second.Identity)
	assert.NotEqual(t, first.Recipient, second.Recipient)
}

func TestGenerateAgeKeypairFailure(t *testing.T) {
	t.Parallel()

	h := &handler{
		temporalClient: &fakeTemporalClient{},
		getenv:         func(string) string { return "" },
		generateAgeIdentity: func(context.Context) (string, string, error) {
			return "", "", errors.New("age-keygen: exec: \"age-keygen\": executable file not found in $PATH")
		},
	}

	recorder := postJSON(t, newMux(h), "/api/age/keygen", nil, nil)
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Error(t, decodeAPIError(t, recorder))

	assert.NotContains(t, recorder.Body.String(), "executable file not found")
	assert.NotContains(t, recorder.Body.String(), "age-keygen")
}

// TestGenerateAgeKeypairNeverLogsPrivateKey replaces the slog default, so it
// cannot run in parallel.
func TestGenerateAgeKeypairNeverLogsPrivateKey(t *testing.T) {
	var logs bytes.Buffer

	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))

	t.Cleanup(func() { slog.SetDefault(previous) })

	mux := newMux(newHandler(&fakeTemporalClient{}, func(string) string { return "" }))

	recorder := postJSON(t, mux, "/api/age/keygen", nil, nil)
	require.Equal(t, http.StatusOK, recorder.Code)

	var response AgeKeygenResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.NotEmpty(t, response.Identity)

	failing := &handler{
		temporalClient: &fakeTemporalClient{},
		getenv:         func(string) string { return "" },
		generateAgeIdentity: func(context.Context) (string, string, error) {
			return "", "", errors.New("simulated age-keygen failure")
		},
	}
	postJSON(t, newMux(failing), "/api/age/keygen", nil, nil)

	assert.NotContains(t, logs.String(), response.Identity)
	assert.NotContains(t, logs.String(), "AGE-SECRET-KEY")
}
