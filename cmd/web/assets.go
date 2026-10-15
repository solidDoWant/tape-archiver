package main

import (
	"embed"
	"io/fs"
)

// embeddedDist embeds the built frontend, which Vite writes here because the
// embed directive cannot reach outside cmd/web. The `all:` prefix lets the
// package compile before any frontend build, when dist/ holds only .gitkeep.
//
//go:embed all:dist
var embeddedDist embed.FS

// distFS returns the embedded SPA build rooted at index.html's directory.
func distFS() (fs.FS, error) {
	return fs.Sub(embeddedDist, "dist")
}
