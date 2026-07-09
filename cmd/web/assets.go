package main

import (
	"embed"
	"io/fs"
)

// embeddedDist embeds the built frontend. web/vite.config.ts configures
// `npm run build`'s output directory to be this dist/ directly (not web/dist)
// because a go:embed pattern can only reach the embedding file's own
// directory subtree, so the SPA build output has to land inside cmd/web/.
//
// dist/ is gitignored except for a committed dist/.gitkeep placeholder (see
// that file): the `all:` prefix lets this directive embed a directory
// containing only a dot-prefixed file, so `go build`, `go vet`,
// `golangci-lint`, and `go test ./...` all compile this package even before
// any frontend build has run. Once a real `npm run build` has populated
// dist/, that is what gets embedded and shipped.
//
//go:embed all:dist
var embeddedDist embed.FS

// distFS returns the embedded SPA build rooted so index.html sits at its
// root (embeddedDist itself is rooted one level up, at "dist"). Before a real
// frontend build has run, the returned FS contains only the placeholder
// dist/.gitkeep and no index.html; webserver.NewHandler treats that as a
// startup error rather than silently serving an empty shell.
func distFS() (fs.FS, error) {
	return fs.Sub(embeddedDist, "dist")
}
