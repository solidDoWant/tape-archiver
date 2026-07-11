// This file implements GET /api/config/schema (issue #279): the committed
// run-config JSON Schema (schemas/run-config.schema.json), served verbatim
// so the web UI's config page can validate a Form-mode-built or
// JSON-mode-pasted config against the exact same schema `make
// generate-schema` produces from internal/config's own types, without a
// second, hand-duplicated copy of its constraints that could drift.
package runsapi

import (
	"net/http"

	"github.com/solidDoWant/tape-archiver/schemas"
)

// getConfigSchema implements GET /api/config/schema: the committed run-config
// JSON Schema document, unmodified. It carries no run/Temporal state, so
// unlike every other handler in this package it needs no Temporal RPC and no
// request timeout.
func (h *handler) getConfigSchema(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(schemas.RunConfigSchema())
}
