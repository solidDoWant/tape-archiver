# web

The tape-archiver web UI frontend: React + TypeScript, built with Vite,
styled with Tailwind CSS. See `docs/web-ui-design.md` at the repo root for the
overall design and `../cmd/web` for the Go server that embeds this app's
build output.

Run via the repo root `Makefile` (`make build` / `make test` / `make lint`),
which wraps `npm ci`/`npm run build`/`npm test`/`npm run lint`/`npm run
typecheck` so `nix develop` is the only prerequisite. `npm run build` writes
its output into `../cmd/web/dist`, not `./dist` — see `vite.config.ts` and
`../cmd/web/assets.go` for why.
