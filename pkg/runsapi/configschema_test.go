package runsapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/schemas"
)

// TestGetConfigSchema checks GET /api/config/schema returns exactly the
// committed run-config JSON Schema (schemas.RunConfigSchema — the same bytes
// `make generate-schema` writes to schemas/run-config.schema.json), so the
// web UI's config page can never validate against a copy that has drifted
// from it.
func TestGetConfigSchema(t *testing.T) {
	t.Parallel()

	mux := newMux(newHandler(&fakeTemporalClient{}, func(string) string { return "" }))

	request := httptest.NewRequest(http.MethodGet, "/api/config/schema", nil)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
	assert.Equal(t, schemas.RunConfigSchema(), recorder.Body.Bytes())

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &decoded))
	assert.Equal(t, "#/$defs/Config", decoded["$ref"])
}
