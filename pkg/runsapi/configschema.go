// GET /api/config/schema (issue #279): the committed run-config JSON Schema,
// served verbatim so the UI validates against it rather than a drifting copy.
package runsapi

import (
	"net/http"

	"github.com/solidDoWant/tape-archiver/schemas"
)

func (h *handler) getConfigSchema(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(schemas.RunConfigSchema())
}
