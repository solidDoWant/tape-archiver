// This file implements POST /api/age/keygen (issue #279): generates a fresh
// age native post-quantum identity (agewrap.GenerateIdentity) for the web
// UI's config page encryption step. The private identity is returned in this
// one response and nowhere else — it is never logged (this file never logs
// the response body, only agewrap's own error text on failure, which never
// carries key material — see GenerateIdentity's doc comment) and never
// persisted server-side; cmd/web holds no session store or database for it
// to land in even by accident (SPEC §4.2).
package runsapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// keygenTimeout bounds age-keygen -pq. Generating an ML-KEM-768 + X25519
// keypair is a fast, local, CPU-bound operation with no network or Temporal
// RPC involved, so a short timeout is enough to catch a hung or missing
// binary without making an operator wait on the config page.
const keygenTimeout = 10 * time.Second

// AgeKeygenResponse is the POST /api/age/keygen response body: a freshly
// generated age post-quantum recipient and its identity. The identity is
// returned exactly once — there is no way to retrieve it again from this API
// afterward — so the client must capture it immediately (the config page's
// encryption step shows it once with a copy control and an explicit
// "store this now" warning, per issue #279's acceptance criteria).
type AgeKeygenResponse struct {
	Recipient string `json:"recipient"`
	Identity  string `json:"identity"`
}

// generateAgeKeypair implements POST /api/age/keygen.
func (h *handler) generateAgeKeypair(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), keygenTimeout)
	defer cancel()

	identity, recipient, err := h.generateAgeIdentity(ctx)
	if err != nil {
		// err here is age-keygen's own tool-failure text (e.g. the binary is
		// missing or the context deadline was exceeded) — agewrap.GenerateIdentity
		// only returns a non-empty identity on success, so there is never key
		// material in this branch to accidentally log.
		slog.ErrorContext(ctx, "runsapi: age keypair generation failed", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Errorf("generate age keypair: %w", err))

		return
	}

	// The response body carries a one-time private key: forbid any cache
	// (browser, proxy, or otherwise) from retaining a copy. POST responses
	// are not normally cached anyway, but this body uniquely deserves the
	// explicit directive — a cached copy would silently break the
	// "displayed once, never retrievable again" contract. Pragma covers
	// legacy HTTP/1.0 intermediaries.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	writeJSON(w, http.StatusOK, AgeKeygenResponse{Recipient: recipient, Identity: identity})
}
