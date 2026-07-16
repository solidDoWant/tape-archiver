// Package webserver provides the HTTP handler for cmd/web: the embedded
// single-page app plus the /api/* JSON API (pkg/runsapi and later
// sub-issues). It knows nothing about Temporal itself, OIDC, or building the
// API handler — those are the caller's job (cmd/web) — this package's only
// job is to mount the two together with the SPA catch-all lowest-priority,
// so cmd/web itself stays a thin wrapper that parses configuration and runs
// an *http.Server.
//
// Liveness/readiness are deliberately not served here: cmd/web wires
// pkg/health on its own dedicated address instead (HEALTH_ADDR), the same
// pattern cmd/worker uses. An earlier version of this package hand-rolled
// its own always-200 /healthz on the main listener; that duplicated
// pkg/health's liveness endpoint with different (weaker — no readiness)
// semantics on a different port than every other binary in this repo probes,
// so it was dropped once cmd/web grew a real health server.
package webserver

import (
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// NewHandler builds the HTTP handler served by cmd/web:
//
//   - /api/ — delegated to api, when non-nil. Registered before the SPA
//     catch-all so it takes priority (Go's http.ServeMux matches the most
//     specific pattern; "/" is the least specific pattern possible, so
//     ordering here is for readability, not correctness).
//   - everything else — the single-page app rooted at assets. A request for a
//     path that is not a real file under assets falls back to index.html so
//     client-side routing (React Router, added in a later sub-issue) keeps
//     working across a hard refresh or a deep link.
//
// assets is injected (rather than the package reaching for a package-level
// embed.FS itself) so it is unit-testable against an in-memory filesystem,
// independent of any real frontend build; cmd/web supplies the actual
// embedded SPA build output. api is injected similarly so this package never
// imports Temporal or knows how /api/* is implemented; pass nil to omit the
// API entirely (e.g. a test that only cares about SPA serving). NewHandler
// fails if assets has no index.html at its root, since that means the SPA
// was not actually built.
func NewHandler(assets fs.FS, api http.Handler) (http.Handler, error) {
	spaHandler, err := newSPAHandler(assets)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	if api != nil {
		mux.Handle("/api/", api)
	}

	mux.Handle("/", spaHandler)

	return mux, nil
}

// newSPAHandler returns a handler serving assets as a single-page app: real
// files are served as themselves, and any other path falls back to
// index.html. It errors up front if assets has no index.html, so a
// misconfigured or unbuilt embed fails at startup rather than on first
// request.
func newSPAHandler(assets fs.FS) (http.Handler, error) {
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return nil, fmt.Errorf("SPA assets: missing index.html at root: %w", err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := assetName(r.URL.Path)

		// fs.Stat succeeds for directories too (e.g. Vite's "assets" output
		// directory), and serving those with http.ServeFileFS redirects to a
		// directory listing instead of falling back to the SPA shell — so the
		// fallback must also require a regular file, not just an existing path.
		if info, err := fs.Stat(assets, name); err != nil || info.IsDir() {
			// A miss under the build's hashed-asset directory is a stale asset
			// (e.g. a browser requesting a previous deploy's index-OLD.js after
			// a rollout), not a client-side route. Returning index.html for it
			// would answer with text/html and a 200, so the browser rejects it
			// as a wrong-MIME script/style instead of treating it as a clean
			// cache miss — 404 is the honest answer. Only non-asset paths fall
			// back to the SPA shell for client-side routing.
			if strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r)

				return
			}

			name = "index.html"
		}

		http.ServeFileFS(w, r, assets, name)
	}), nil
}

// assetName maps a request path to a name relative to the assets root, as
// expected by fs.Stat/http.ServeFileFS (no leading slash; "index.html" for
// the root).
func assetName(requestPath string) string {
	name := strings.TrimPrefix(path.Clean(requestPath), "/")
	if name == "" || name == "." {
		name = "index.html"
	}

	return name
}
