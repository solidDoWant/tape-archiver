# Web UI — Design

Status: **approved 2026-07-09** · Epic: #239 · Branch: `feature/web-ui`

## 1. Goal

A browser UI for operators to submit and monitor backup runs (including dry-runs),
act on operator-in-the-loop pauses, and browse run history — without shelling in to
use `tapectl`. It is an *additional* front door to Temporal; `tapectl` remains fully
supported and the workflow contract (`workflows/backup/contract.go`) stays the shared
source of truth.

## 2. Scope

In scope:

- **Submit runs**: upload/paste a run config (JSON), server-side validation with the
  same `internal/config` code `tapectl` uses, dry-run toggle (mhvtl override), submit
  to Temporal.
- **Monitor runs**: live run detail — execution status, last completed phase (via the
  existing `LastCompletedPhaseQuery`), phase timeline, pause state — updating via SSE.
- **Operator pause actions**: when a run is paused (Eject I/O-station, Load/Write
  failure, Burn pause), show the pause prominently and offer Resume / Abort, wired to
  the existing `OperatorResumeSignal` / `OperatorAbortSignal`.
- **Run history**: list past executions of the singleton `backup` workflow from
  **Temporal visibility only**, with status, timing, and phase reached. History depth
  is whatever Temporal retention keeps — this is a deliberate decision (owner call,
  2026-07-09) to preserve SPEC §4.2 (no cross-run state, no online catalog). PDF
  reports remain Discord-delivered; the UI shows run metadata, not report files.
- **Auth**: OIDC authorization-code flow in the Go backend against the environment's
  existing IdP (issuer/client configurable; no IdP-specific code), encrypted session
  cookie, all API and page routes gated.
- **Polish bar**: responsive layout, dark mode, reviewed screenshots.

Out of scope (explicitly):

- Library/tape inventory views (not selected; would need new data-queue activities).
- Report/PDF storage or serving, and any persistent store owned by the UI (SPEC §4.2).
- Editing anything on the storage host; the UI talks to **Temporal only**.
- Multi-run concurrency changes — runs remain a singleton (`backup` workflow ID).

## 3. Architecture

One new binary, `cmd/web`: a Go HTTP server that

- serves the built SPA from `go:embed`,
- exposes a JSON API under `/api/*`, backed by the existing
  `pkg/temporalclient` (same `TEMPORAL_*` envconfig as `tapectl`/workers),
- exposes `/api/events/…` SSE streams (server polls Temporal describe/query at a
  short interval, pushes deltas; no client polling storms),
- handles OIDC login/callback/logout and session middleware,
- serves `/healthz` (liveness) and Prometheus metrics on the existing
  `pkg/metrics` pattern.

The web service is **stateless** (sessions live in the cookie, encrypted with a
configured key), so the Deployment can scale or restart freely and it holds no
cross-run state.

### API sketch

| Route | Method | Backing |
|-------|--------|---------|
| `/api/runs` | GET | Visibility `ListWorkflowExecutions` (workflow ID `backup`), newest first |
| `/api/runs/{runID}` | GET | `DescribeWorkflowExecution` + `LastCompletedPhaseQuery` + pause state |
| `/api/runs` | POST | Validate config (`internal/config`), `ExecuteWorkflow` (honors dry-run flag) |
| `/api/runs/{runID}/resume` | POST | `OperatorResumeSignal` |
| `/api/runs/{runID}/abort` | POST | `OperatorAbortSignal` |
| `/api/events/runs/{runID}` | GET (SSE) | Poll-and-push of run detail |
| `/api/me` | GET | Session identity |

Pause-state visibility may need a small additive workflow query (e.g.
`CurrentPauseQuery`) in `workflows/backup` — additive and backward-compatible;
`tapectl` is untouched.

## 4. Stack

- **Backend**: Go (stdlib `net/http` mux), `coreos/go-oidc/v3` + `golang.org/x/oauth2`.
  Same module, same lint/test tooling.
- **Frontend**: React + TypeScript, Vite, Tailwind CSS, TanStack Query, React Router.
  Vitest + Testing Library for unit tests. Playwright for e2e. `web/` directory at the
  repo root; `npm` with a committed lockfile (Nix `buildNpmPackage`-compatible).
- **Build**: `make build` builds the SPA and embeds `web/dist` into `cmd/web`;
  `make test` / `make lint` grow frontend equivalents (vitest, `tsc --noEmit`,
  eslint) so the existing merge gates cover the new surface. Node toolchain added to
  `flake.nix` devshell.

## 5. Deployment

- **Image**: new Nix `streamLayeredImage` (`tape-archiver-web`), built by
  `make build-images` alongside the workers.
- **Chart**: new Helm chart `deploy/charts/tape-archiver-web` — Deployment, Service,
  optional Ingress (cert-manager-friendly), OIDC + Temporal config via values +
  existing-Secret references. `make helm` packages it; `make chart-lint` renders it.
- Runs in Kubernetes next to the control worker; needs network reach to Temporal
  frontend and the IdP only.

## 6. Testing

- Unit: Go (`-race`, testify, table-driven) and vitest — in `make test`.
- Integration (`//go:build integration`): API handlers against dev Temporal
  (`make temporal-up`), env-skipped as usual.
- e2e: Playwright drives the real UI against mhvtl + dev Temporal (dry-run submit →
  watch phases → history shows the run), wired into `make test-e2e`.
- Acceptance criteria per sub-issue are Given/When/Then, observable behavior only.

## 7. Definition of done (epic acceptance)

Operators can, from a browser: submit and monitor runs (incl. dry-run), act on
operator pauses, and browse run history (Temporal-retention-bound). OIDC-protected.
e2e tested against mhvtl + dev Temporal. Documented under `docs/`. Deployable via its
own Helm chart and image. Polish bar: responsive, dark mode, reviewed screenshots.

## 8. Delivery plan (sub-issues)

Foundation lands first, then parallel feature work; all PRs target `feature/web-ui`.

1. **Scaffolding**: `cmd/web` skeleton, `web/` Vite app, embed pipeline, Makefile +
   flake integration, lint/test gates green end-to-end.
2. **API core**: Temporal wiring, runs list + run detail endpoints, health/metrics.
3. **Submit + dry-run** (API + form UI, validation errors surfaced).
4. **Live monitoring** (SSE + run detail page, phase timeline).
5. **Pause actions** (pause query if needed, resume/abort UI with confirmation).
6. **OIDC auth** (middleware, sessions, login flow).
7. **App shell + polish** (routing, responsive, dark mode, design pass).
8. **Image + Helm chart** (+ deploy docs).
9. **e2e suite** (Playwright, `make test-e2e`).
10. **Docs + screenshots** (`docs/web-ui.md`, final polish).

## 9. Decisions log

- 2026-07-09 (owner): history/report source = **Temporal visibility only**; no report
  archive, no UI-owned storage. Reports remain Discord-delivered.
- 2026-07-09 (owner): stack = React/TS SPA + Go API; auth = OIDC; deployment =
  separate image + separate chart.
- 2026-07-09 (owner): library/tape inventory views are out of scope for this epic.
