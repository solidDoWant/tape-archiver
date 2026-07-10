# CLAUDE.md

Operating manual for working in this repository. tape-archiver backs up ZFS snapshots
to LTO tape for long-term, offline cold storage. Read `SPEC.md` first — it is the
authoritative technical reference for architecture, the run model, package contracts,
and configuration.

## Things to Remember

- Stop and ask when a human can reasonably resolve a blocker themselves, and whenever
  you are uncertain. Do not guess at requirements.
- When you skip a verification step, say so with a specific, actionable reason naming
  the exact constraint (e.g. "Skipping tape-write test — no tape drive or mhvtl device
  available in this environment").
- Do not use bare `<` / `>` as placeholders in Markdown (some renderers drop them); use
  backticks or HTML entities. Markdown paragraphs have no internal hard line breaks.

## Project Goals and Principles

This project exists to make ZFS snapshots recoverable in ~20 years from tape alone.
Four principles are non-negotiable and override convenience (see `SPEC.md` §2):

1. **~20-year recoverability** — open standards only (`tar`, `age`, PAR2, LTFS, ISO),
   error correction, and recovery tooling shipped on the recovery disc.
2. **Minimize tape wear** — feed drives above the speed-matching floor so they stream
   without shoe-shining, and *prove* the rate by benchmark.
3. **Plan for media failure** — multiple copies on multiple tapes, per-archive PAR2,
   bounded blast radius.
4. **Everything is tested** — if it is not tested, it does not work.

When a change touches any of these, call it out explicitly.

## Tech Stack

- **Go 1.26**, single module (`go.mod` at the repo root). The one exception is
  `web/go.mod`: a deliberately empty nested module that fences the `web/` npm project
  (and its `node_modules`, which can ship incidental `.go` files) out of the root
  module's `./...` package walk — see `web/go.mod`'s doc comment. It carries no real
  Go code and is not part of the single-module Go surface described elsewhere in this
  file.
- **Temporal** for orchestration. Two task queues: `control` (runs in Kubernetes) and
  `data` (runs as a container on the storage host, where the bulk data lives).
- **External tooling** bundled in the data worker image at pinned versions, matching the
  recovery disc: `ltfs`/`mkltfs`, `age` (>= 1.3.1, native post-quantum recipients),
  `par2` (par2cmdline-turbo), `zstd`, `mt-st`, `sg3-utils`, `lsscsi`.
- **`mhvtl`** virtual tape library for dry-run and integration testing.
- **Nix flakes** for the dev environment and for building OCI images
  (`streamLayeredImage`). **`golangci-lint`** for linting.

## Project Structure

See `SPEC.md` §15 for the full layout. In brief:

- `cmd/` — binaries: `worker` (Temporal worker; role selects control vs data),
  `tapectl` (CLI to submit/inspect runs), `gen-config-schema`, `web` (the web UI's
  Go server), `webdevoidc`/`webdevseed` (local-only `make web-dev` dev tooling —
  a standalone fake OIDC provider and sample-run seeder, never deployed).
- `pkg/` — one concern per package (tape/changer, ltfs, age, par2, tar, zfs, k8s
  snapshot discovery, PDF report, recovery ISO, Discord webhook, checksums, logging,
  metrics, temporal client).
- `internal/config` — run-config types and env parsing.
- `workflows/backup/` — the backup workflow and activities, split by concern, with
  co-located tests.
- `schemas/` — generated JSON config schema (committed). `deploy/charts/` — Helm charts.
  `docs/` — operator docs. `e2e/` — end-to-end tests. `bin/` — build output.
- `.claude/` — slash commands and gitignored per-issue task files.

## Commands

Build tooling follows media-processor (Nix + a single Makefile). Targets (to be created
as the project is implemented; keep this list current):

- `make build` — build binaries into `bin/`.
- `make fmt` / `make vet` / `make lint` (`lint-fix`) — format, vet, lint.
- `make test` — unit tests, `-race`.
- `make test-integration` — integration tests against `mhvtl` + dev Temporal.
- `make test-e2e` — end-to-end tests.
- `make benchmark` — write-rate / shoe-shining benchmarks (real hardware).
- `make generate-schema` — regenerate the committed config JSON schema.
- `make update-dependencies` — update deps, `go mod tidy`, refresh Nix vendor hashes.
- `make build-images` — build the data-worker, control-worker, and web OCI images via
  Nix.
- `make helm` — package the control-worker and web Helm charts into `bin/helm/`
  (`PUSH_ALL=true` also pushes them to the OCI chart registry).
- `make build-all` — build all worker/web images and package both charts in one
  command.
- `make release` — cut the `v$(VERSION)` git tag + GitHub release (dry run unless
  `PUSH_ALL=true`; requires an authenticated `gh`).
- `make chart-lint` — fetch deps, lint, and render both the control-worker and web
  Helm charts (`deploy/charts/`); no cluster required.
- `make temporal-up` / `make temporal-down` — local Temporal for integration tests.
- `make mhvtl-up` / `make mhvtl-down` — virtual tape library for tests/dry-run.
- `make zpool-up` / `make zpool-down` — ephemeral file-backed ZFS pool for `pkg/zfs`
  integration tests. The flake builds a version-matched ZFS kernel module
  (`$ZFS_MODULES`); `zpool-up` loads it at runtime (needs `sudo`), falling back to
  the host's own module when the flake build does not match the running kernel.
- `make web-dev` / `make web-dev-down` — one-command local web UI: dev Temporal +
  mhvtl + ZFS pool + a local-only fake OIDC provider (`cmd/webdevoidc`) + real
  control/data workers, seeded with a few sample dry-run backups
  (`cmd/webdevseed`), `cmd/web` running in the foreground. Interrupting it
  (Ctrl+C/SIGINT or SIGTERM) waits for `cmd/web` to shut down gracefully, then
  runs the full `web-dev-down` teardown automatically, so every `make web-dev`
  starts from a clean slate; `make web-dev-down` itself remains the remedy
  after a crash/SIGKILL, which cannot be trapped. See `docs/web-ui.md`'s
  "Local development" section.

## Dev Tools

Provisioned via `flake.nix`; run `nix develop` if something is missing. The storage host
is reachable with `tsh ssh root@ubuntu-storage-host-01` (Teleport; `tsh` is in the dev
shell). The pool is mounted locally at `/mnt/bulk-pool-01`. In isolated "coder"
environments you may install/update tools directly without asking.

## Hardware and Safety

Tape and library operations are physical and partly irreversible — treat them with care.

- **Never write to a non-blank tape.** Before `mkltfs`, verify the loaded tape is
  blank/empty. A run must never silently overwrite existing data.
- **Never compute during the write window.** All tape contents are staged and verified
  on disk before any write begins (`SPEC.md` §4.3). Do not introduce inline work between
  the source and the drive — it risks shoe-shining and tape wear.
- **Prefer the non-rewinding device nodes** (`/dev/nst0`, `/dev/nst1`).
- **Default to dry-run** (`mhvtl`-backed) when developing or testing anything that
  touches the library; only target real devices deliberately.
- The run config is the single source of truth; there is no cross-run state. Do not add
  hidden persistent state or online catalogs (`SPEC.md` §4.2).

## Acceptance Criteria Rules

- Criteria are Given/When/Then describing **observable behavior only** — no
  implementation details.
- **Never modify acceptance tests to make them pass — fix the code.**
- Mark a criterion complete only after running its test and confirming it passes.
- When generating tests, exercise public interfaces only; never mock the component under
  test. Every test must fail if its criterion is violated.

## Testing Style

- `testify` (`require` + `assert`). Prefer table-driven tests. In table structs use a
  `require.ErrorAssertionFunc` field defaulting to `require.NoError`, overridden per
  case; assert non-error values with `assert.Equal` on concrete types.
- Loop variables are full singular words (`snapshot`, not `snap`).
- Integration tests use `//go:build integration` and skip when required env vars or
  devices (Temporal, `mhvtl`, real tape hardware) are absent. Real-hardware and
  benchmark tests are similarly build-tag gated and env-skipped.
- Always use `t.Context()`, never `context.Background()`.
- The full tape path is testable without real tapes via `mhvtl` — there is no excuse for
  untested library/LTFS logic.

## Documentation

Any change to a user-facing surface (run-config fields, CLI flags, env vars, metrics,
the report or ISO contents, the Discord payload) updates the relevant doc under `docs/`
and, if it changes the run config, regenerates the schema (`make generate-schema`) — in
the same change. Keep `SPEC.md` current with any behavioral change it describes.

## Quality Rules

- Pass all acceptance tests before marking work complete; fix code, not tests.
- Implement minimally to meet the criteria; respect each issue's stated non-goals.
- All work lands via PRs with `Fixes #<issue>` in the body. Never push directly to
  `master`.
- If an issue contradicts these rules or `SPEC.md`, flag it in a comment rather than
  silently resolving it.

## Context Management

Start each session by reading the assigned issue and `.claude/tasks/$ISSUE_NUMBER.md`
(if present). When context runs low, save progress to that task file. Post significant
decisions as issue comments so they persist. Start fresh sessions for new work items.

## Task File Lifecycle

`.claude/tasks/$ISSUE_NUMBER.md` files are local, gitignored scratch and must never be
committed. Delete the task file as the final step when an issue is merged or closed; if
an issue is reopened, delete any stale task file first.
