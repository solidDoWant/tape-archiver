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
- 2026-07-10 (owner, redesign epic #271): implement the Claude Design canvas designs;
  self-hosted fonts (npm `@fontsource` packages, Vite-bundled — no font CDN); one
  canonical token set using the app design file's values where the login design
  drifts; no literal IPs in footer/sample content; footer version/host line is
  deploy-configurable and hidden entirely when unset; dark + light both required;
  narrow viewports adapt the fixed-width desktop design by stacking (no separate
  mobile design).
- 2026-07-10 (#272): design tokens are CSS custom properties
  (`web/src/design/tokens.css`) registered as Tailwind v4 theme values via
  `@theme inline`; dark mode keeps the existing class-based `.dark` mechanism
  (theme.ts) rather than the design's `data-theme` attribute. Theme control extended
  to three-way Light/Dark/Auto (an explicit `auto` preference persisted in
  localStorage).
- 2026-07-10 (#272): unauthenticated page requests are now served the SPA (which
  renders a styled login page at `/login`) instead of pkg/webauth 302-ing straight to
  the IdP; the OIDC callback surfaces failures to that page via
  `/login?error=denied|expired` (denied = the IdP's own `error` param; expired =
  every other validation failure). `/api/*` 401 JSON behavior is unchanged, and all
  state/nonce/PKCE/redirect-sanitization properties are preserved (the SPA mirrors
  `sanitizeRedirectPath` client-side; the server still re-validates).
- 2026-07-10 (#272): the login button reads "Continue with SSO" — a per-provider
  display name (the design's `providerName` prop, default "Authentik") would need a
  new config knob (e.g. `OIDC_PROVIDER_LABEL`) that issue #272 doesn't call for;
  deferred until something else needs it.
- 2026-07-10 (#272): footer version comes from the binary's embedded VCS build info
  (`internal/buildinfo.ToolVersion`) served by a new ungated `GET /api/build-info`;
  the optional host label is the new `WEB_FOOTER_HOST` env var (omitted from the
  response — and the footer — when unset). Never the design's hardcoded
  `v0.4.1`/`homelab · 10.0.0.4` samples.
- 2026-07-10 (#272): icons are a small set of hand-drawn inline SVG components
  (`web/src/icons.tsx`) replacing the design's bare Unicode glyphs — no icon-library
  dependency until the fuller redesign surface justifies one.
- 2026-07-10 (#272): interim nav mapping until the dashboard/tapes redesign issues
  land: sidebar "Dashboard" routes to `/history` (the existing run-history view),
  "Start new run" to `/` (the existing JSON submit form), "Tapes" to a minimal
  placeholder page. The sidebar's active-run check ("Start new run" disabled while a
  run is in progress) is a one-shot `GET /api/runs` at shell mount, not a live
  subscription — accepted minimal-scope gap for the foundation issue.
- 2026-07-10 (#278): the real Tapes page replaces #272's placeholder. It ships only
  the design's second, history-resolved table (`DESIGN_ANALYSIS.md` §2 "C. Tapes");
  the design's "IN THE LIBRARY NOW" live-changer-element table is dropped entirely
  per the epic's explicit non-goal — it would need a live SCSI element-status
  endpoint (`READ ELEMENT STATUS` via `SG_IO`, reachable only from the storage host,
  not the control-plane web pod) that this epic never builds.
- 2026-07-10 (#278): no limit/"show more" control on the page — it uses `GET
  /api/tapes`'s own default (the 50 most recent runs, `tapes.go`'s
  `defaultListTapesRunLimit`) as-is. The reference design has no such control on this
  page either, so there is nothing to reproduce. The page copy discloses the cap
  (banner and footer both say "50 most recent runs") so "derived from history" is
  never read as "everything still within Temporal retention".
- 2026-07-10 (#278, review): the write-health cell renders TapeAlert, below-floor,
  and repositions as independent, simultaneous badges (matching the design's
  "healthy / below floor / N repositions" badge set and `FinalTapeCard`'s
  precedent) — the three are independent dimensions of
  `backup.WriteHealth.Healthy()`, so one badge must never suppress another, and a
  tape unhealthy solely from repositions must not look healthy. When repositions
  could not be measured at all, the cell says "repositions not measured" explicitly
  rather than implying zero. A failed tape's reason renders as visible text under
  the outcome badge, not a hover-only title.
- 2026-07-10 (#278): row order is exactly what `GET /api/tapes` already returns
  (newest run first, then logical-tape index, then copy index) — no client-side
  resort, keeping the page's grouping identical to the API contract.
- 2026-07-10 (#278): a degraded run (one whose history could not be reconstructed,
  `runErrors[]`) renders as a non-fatal warning notice listing the run ID and reason,
  separate from and never hiding the tapes successfully derived from every other
  run — matching the aggregate endpoint's explicit per-run degrade contract (issue
  #273).
- 2026-07-10 (#278): shipped without new screenshots — no live `make web-dev`
  environment was exercised for this change; the epic's consolidated screenshot pass
  happens in issue #281 alongside the other new-page issues.
