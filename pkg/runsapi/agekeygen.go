// POST /api/age/keygen (issue #279): generates a fresh age post-quantum
// identity. The private identity is returned only in this response and is
// never logged or persisted server-side (SPEC §4.2).
package runsapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// keygenTimeout catches a hung or missing age-keygen binary; generation itself is fast and local.
const keygenTimeout = 10 * time.Second

// AgeKeygenResponse is the POST /api/age/keygen response body. The identity
// is returned exactly once and cannot be retrieved again.
type AgeKeygenResponse struct {
	Recipient string `json:"recipient"`
	Identity  string `json:"identity"`
}

func (h *handler) generateAgeKeypair(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), keygenTimeout)
	defer cancel()

	identity, recipient, err := h.generateAgeIdentity(ctx)
	if err != nil {
		// GenerateIdentity only returns an identity on success, so err never
		// carries key material.
		slog.ErrorContext(ctx, "runsapi: age keypair generation failed", "error", err)
		// The tool error can leak internals (binary path, flags), so the client
		// gets a generic message.
		writeError(w, http.StatusInternalServerError, errors.New("could not generate an age keypair"))

		return
	}

	// A cached copy of the one-time private key would break "displayed once".
	// Pragma covers HTTP/1.0 intermediaries.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	writeJSON(w, http.StatusOK, AgeKeygenResponse{Recipient: recipient, Identity: identity})
}
