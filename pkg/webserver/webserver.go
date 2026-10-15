// Package webserver builds cmd/web's HTTP handler: the embedded single-page
// app plus the /api/* JSON API. Health endpoints are served separately by
// pkg/health on HEALTH_ADDR.
package webserver

import (
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// NewHandler mounts api (when non-nil) under /api/ and serves the SPA in
// assets for everything else, falling back to index.html for client-side
// routes. It fails if assets has no index.html, i.e. the SPA was not built.
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

// SecurityHeaders wraps next with hardening response headers; cmd/web mounts
// it outermost so every response gets them.
//
// Framing is forbidden (X-Frame-Options plus CSP frame-ancestors) not only
// against clickjacking: a click inside a framed same-origin page reports
// Sec-Fetch-Site: same-origin, which pkg/webauth's CSRF guard trusts.
// Referrer-Policy keeps URLs carrying run IDs from leaking to third parties.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Frame-Options", "DENY")
		header.Set("Content-Security-Policy", "frame-ancestors 'none'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")

		next.ServeHTTP(w, r)
	})
}

func newSPAHandler(assets fs.FS) (http.Handler, error) {
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return nil, fmt.Errorf("SPA assets: missing index.html at root: %w", err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := assetName(r.URL.Path)

		// Directories must fall back too: ServeFileFS would redirect to a listing.
		if info, err := fs.Stat(assets, name); err != nil || info.IsDir() {
			// A miss under assets/ is a stale hashed asset from a previous
			// deploy, not a route; serving index.html would yield a wrong-MIME
			// 200 instead of a clean 404.
			if strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r)

				return
			}

			name = "index.html"
		}

		http.ServeFileFS(w, r, assets, name)
	}), nil
}

// assetName maps a request path to an fs.FS name ("index.html" for the root).
func assetName(requestPath string) string {
	name := strings.TrimPrefix(path.Clean(requestPath), "/")
	if name == "" || name == "." {
		name = "index.html"
	}

	return name
}
