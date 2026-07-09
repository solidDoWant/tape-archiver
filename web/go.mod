// This go.mod deliberately has no Go code of its own. web/ is an npm
// project (the React SPA embedded by cmd/web); some npm packages
// (e.g. flatted) ship an incidental Go implementation under
// node_modules/ that has nothing to do with this project. Go's `./...`
// pattern excludes nested modules, so this file fences that whole
// subtree out of `go vet ./...`, `go test ./...`, and
// `golangci-lint run ./...` at the repo root without needing per-tool
// exclude-dir configuration.
module tape-archiver-web-excluded-from-go-module

go 1.26
