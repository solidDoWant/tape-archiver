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

// TestGenerateAgeKeypairShape checks the real endpoint (real age-keygen
// binary, via the same DI newHandler wires production to) returns a
// well-formed post-quantum recipient/identity pair.
func TestGenerateAgeKeypairShape(t *testing.T) {
	t.Parallel()

	mux := newMux(newHandler(&fakeTemporalClient{}, func(string) string { return "" }))

	recorder := postJSON(t, mux, "/api/age/keygen", nil, nil)
	assert.Equal(t, http.StatusOK, recorder.Code)

	var response AgeKeygenResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))

	assert.True(t, strings.HasPrefix(response.Recipient, "age1pq1"), "recipient must be post-quantum: %q", response.Recipient)
	assert.True(t, strings.HasPrefix(response.Identity, "AGE-SECRET-KEY-PQ-1"), "identity must be post-quantum: %q", response.Identity)
}

// TestGenerateAgeKeypairIsFreshEveryCall checks POST /api/age/keygen never
// caches or replays a previously generated keypair — a client that calls it
// twice (e.g. the operator clicking "Generate new age keypair" again) always
// gets a brand-new pair, matching the "displayed once, never retrievable
// again" semantics the config page's encryption step relies on.
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

// TestGenerateAgeKeypairFailure checks a failure generating the keypair (the
// injected dependency here stands in for e.g. a missing age-keygen binary)
// is reported as a 500 with a plain error, never a keypair — and,
// critically, never with a 200 that would suggest a client could retry to
// fetch the "same" (i.e. no) key again.
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
}

// TestGenerateAgeKeypairNeverLogsPrivateKey checks that the identity
// returned by a successful POST /api/age/keygen never appears anywhere in
// the process's own logs — CLAUDE.md's "never logged" requirement for the
// generated private key. This installs a temporary slog default handler
// capturing every record, drives one successful request and one failing
// request (the failure path logs the tool error, which must never itself
// carry key material), and asserts the successful response's identity
// substring is absent from the captured log text.
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

	// Also drive the failure path (agekeygen.go's one slog.ErrorContext call)
	// to prove it, too, never has key material to log — GenerateIdentity only
	// ever returns a non-empty identity alongside a nil error.
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
