// Package webserver provides the HTTP handler for cmd/web: the embedded
// single-page app plus a /healthz liveness endpoint. It knows nothing about
// Temporal, OIDC, or /api routes — those land in later sub-issues of the web
// UI epic (docs/web-ui-design.md §8); this package's only job today is to
// serve the built SPA and answer liveness probes, so cmd/web itself stays a
// thin wrapper that parses configuration and runs an *http.Server.
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
//   - /healthz — always 200, independent of assets or any dependency.
//   - everything else — the single-page app rooted at assets. A request for a
//     path that is not a real file under assets falls back to index.html so
//     client-side routing (React Router, added in a later sub-issue) keeps
//     working across a hard refresh or a deep link.
//
// assets is injected (rather than the package reaching for a package-level
// embed.FS itself) so it is unit-testable against an in-memory filesystem,
// independent of any real frontend build; cmd/web supplies the actual
// embedded SPA build output. NewHandler fails if assets has no index.html at
// its root, since that means the SPA was not actually built.
func NewHandler(assets fs.FS) (http.Handler, error) {
	spaHandler, err := newSPAHandler(assets)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.Handle("/", spaHandler)

	return mux, nil
}

// handleHealthz answers liveness probes. It reports the process as live
// unconditionally: it must not fail just because some later dependency (once
// this server grows one) is unreachable, or an orchestrator would restart a
// pod that is merely waiting on that dependency to recover.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
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
