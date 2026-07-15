# Web UI — Design

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
  is whatever Temporal retention keeps — a deliberate decision to preserve SPEC §4.2
  (no cross-run state, no online catalog). PDF reports remain Discord-delivered; the
  UI shows run metadata, not report files.
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
- Acceptance criteria are Given/When/Then, observable behavior only.

## 7. Definition of done

Operators can, from a browser: submit and monitor runs (incl. dry-run), act on
operator pauses, and browse run history (Temporal-retention-bound). OIDC-protected.
e2e tested against mhvtl + dev Temporal. Documented under `docs/`. Deployable via its
own Helm chart and image. Polish bar: responsive, dark mode, reviewed screenshots.

## 8. Delivery plan

Foundation lands first, then parallel feature work:

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

## 9. Design decisions

- History/report source is **Temporal visibility only**; no report archive, no
  UI-owned storage. Reports remain Discord-delivered.
- Stack is a React/TS SPA + Go API; auth is OIDC; deployment is a separate image +
  separate chart.
- Library/tape inventory views are out of scope for the web UI.
- The UI implements the Claude Design canvas designs, with these adaptations:
  self-hosted fonts (npm `@fontsource` packages, Vite-bundled — no font CDN); one
  canonical token set, using the app design file's values where the login design
  drifts; no literal IPs in footer/sample content; the footer version/host line is
  deploy-configurable and hidden entirely when unset; dark + light are both required;
  narrow viewports adapt the fixed-width desktop design by stacking (no separate
  mobile design).
- Design tokens are CSS custom properties (`web/src/design/tokens.css`) registered as
  Tailwind v4 theme values via `@theme inline`; dark mode keeps the existing
  class-based `.dark` mechanism (theme.ts) rather than the design's `data-theme`
  attribute. Theme control is extended to three-way Light/Dark/Auto (an explicit
  `auto` preference persisted in localStorage).
- Unauthenticated page requests are served the SPA (which renders a styled login page
  at `/login`) instead of `pkg/webauth` 302-ing straight to the IdP; the OIDC callback
  surfaces failures to that page via `/login?error=denied|expired` (denied = the IdP's
  own `error` param; expired = every other validation failure). `/api/*` 401 JSON
  behavior is unchanged, and all state/nonce/PKCE/redirect-sanitization properties are
  preserved (the SPA mirrors `sanitizeRedirectPath` client-side; the server still
  re-validates).
- The login button reads "Continue with SSO". A per-provider display name (the
  design's `providerName` prop, default "Authentik") would need a new config knob
  (e.g. `OIDC_PROVIDER_LABEL`) that the current scope doesn't call for; deferred until
  something else needs it.
- The footer version comes from the binary's embedded VCS build info
  (`internal/buildinfo.ToolVersion`) served by a new ungated `GET /api/build-info`;
  the optional host label is the `WEB_FOOTER_HOST` env var (omitted from the
  response — and the footer — when unset). Never the design's hardcoded
  `v0.4.1`/`homelab · 10.0.0.4` samples.
- Icons are a small set of hand-drawn inline SVG components (`web/src/icons.tsx`)
  replacing the design's bare Unicode glyphs — no icon-library dependency until a
  fuller redesign surface justifies one.
- Foundation nav mapping (before the dashboard/tapes redesign landed): sidebar
  "Dashboard" routed to `/history` (the existing run-history view), "Start new run" to
  `/` (the existing JSON submit form), "Tapes" to a minimal placeholder page. The
  sidebar's active-run check ("Start new run" disabled while a run is in progress) is
  a one-shot `GET /api/runs` at shell mount, not a live subscription — an accepted
  minimal-scope gap for the foundation.
- The real Tapes page replaces that placeholder. It ships only the design's
  history-resolved Tapes table; the design's "IN THE LIBRARY NOW" live-changer-element
  table is dropped entirely per the explicit non-goal — it would need a live SCSI
  element-status endpoint (`READ ELEMENT STATUS` via `SG_IO`, reachable only from the
  storage host, not the control-plane web pod) that the web UI never builds.
- The Tapes page has no limit/"show more" control — it uses `GET /api/tapes`'s own
  default (the 50 most recent runs, `tapes.go`'s `defaultListTapesRunLimit`) as-is.
  The reference design has no such control on this page either, so there is nothing to
  reproduce. The page copy discloses the cap (banner and footer both say "50 most
  recent runs") so "derived from history" is never read as "everything still within
  Temporal retention".
- The Tapes write-health cell renders TapeAlert, below-floor, and repositions as
  independent, simultaneous badges (matching the design's "healthy / below floor /
  N repositions" badge set and `FinalTapeCard`'s precedent) — the three are
  independent dimensions of `backup.WriteHealth.Healthy()`, so one badge must never
  suppress another, and a tape unhealthy solely from repositions must not look
  healthy. When repositions could not be measured at all, the cell says "repositions
  not measured" explicitly rather than implying zero. A failed tape's reason renders
  as visible text under the outcome badge, not a hover-only title.
- Tapes row order is exactly what `GET /api/tapes` already returns (newest run first,
  then logical-tape index, then copy index) — no client-side resort, keeping the
  page's grouping identical to the API contract.
- A degraded run (one whose history could not be reconstructed, `runErrors[]`) renders
  as a non-fatal warning notice listing the run ID and reason, separate from and never
  hiding the tapes successfully derived from every other run — matching the aggregate
  endpoint's explicit per-run degrade contract.
- The dashboard lands at `/` (`Dashboard.tsx`), replacing the interim foundation nav
  mapping above — sidebar "Dashboard" now routes to `/`, "Start new run" moved to
  `/submit` (freeing up `/`; the submit form itself is unchanged — a richer dedicated
  flow is a separate, later change). `/history` now redirects (client-side,
  `AuthGate`'s existing route-change effect, `pushState`) to `/` rather than rendering
  under two URLs — `RunHistory.tsx` is deleted, fully superseded by the dashboard's
  embedded, paginated (8/page) `RunsTable.tsx`.
- The current-run card's live status/phase/pause reuses the same SSE subscription
  `RunDetail.tsx`'s run page already had, factored out to a shared `useRunEvents` hook
  (`runEvents.ts`) rather than duplicated — both call sites need identical semantics
  (connecting/live/terminal/error, "don't reset display state on reconnect").
  `RunSummary`/`statusBadgeClass` similarly moved from the now-deleted `RunHistory.tsx`
  to the shared `api.ts`.
- The current-run card's progress bar is derived from the position of the live
  `lastCompletedPhase` in the known, fixed 11-phase pipeline order
  (`workflows/backup`'s `Phase*` constants) — not a second per-tick fetch of the fuller
  `GET /api/runs/{runID}/phases` timeline, and not a fabricated percentage. `GET
  /api/runs` itself is fetched once per dashboard mount (same one-shot, not-live
  pattern as the sidebar's `useActiveRun` — a run that starts while the dashboard is
  already open is not picked up without a reload, an accepted minimal-scope gap
  matching existing precedent); the currently active run's own status/phase/pause stay
  live via the SSE subscription above once known at mount.
- The library card never implies live drive/slot occupancy (no SCSI element-status
  source exists — a redesign non-goal) — it always shows an explicit "not available"
  disclosure for that. Distinct from and in addition to that disclosure, it also shows
  a small history-derived summary (tape outcome counts) from `GET /api/tapes`, clearly
  labeled as derived from run history, never as live state. This reconciles the
  "show an unavailable state" requirement with the "derived/stale" summary card the
  task brief described: the card does both: an explicit unavailable state for the
  live-occupancy concept the design showed, plus an honestly-labeled historical
  summary using data the backend already serves.
- The hardware/environment card sources its device paths/webhook/recipients from `GET
  /api/runs/{runID}/config` for the active run (or, once idle, the most recently
  submitted one) — never hardcoded. A value unset in that config has no row at all
  (never a blank or design-sample placeholder row); no run ever submitted (or a config
  fetch failure, e.g. history aged out of retention) shows an explicit "not reported"
  state instead of the card silently rendering empty.
- The run detail page is built around a phase rail (`PhaseRail.tsx`) + detail pane,
  replacing the old single-pane `RunDetail.tsx`. `RunDetail.tsx` now does a plain `GET
  /api/runs/{runID}` *before* opening the SSE stream, purely to distinguish "run does
  not exist" (404) from a dropped connection — a browser `EventSource` cannot surface
  a failed connection's HTTP status, the same reasoning `LogPanel.tsx` already
  documents for polling instead of SSE. Only once that succeeds does it mount the live
  view (SSE + `GET /api/runs/{runID}/phases`).
- Phase display order/names are `workflows/backup/workflow.go`'s `backupPhases()`
  order verbatim (Resolve, Prepare, Pack, `PhaseGeneratePAR2`, Verify, Load, Write,
  Eject, Report, Burn, Deliver) — the frontend never re-sorts what `GET
  /api/runs/{runID}/phases` returns. The one display transform: `PhaseGeneratePAR2`'s
  Go constant value is literally `"Generate PAR2"` (kept stable for history/logging),
  but the rail/detail pane label it `"PAR2"` per SPEC's terminology (`phaseFormat.ts`'s
  `phaseLabel()`).
- `GET /api/runs/{runID}/phases` returning 410 (aged out of Temporal's retention)
  renders a dedicated "aged out" empty state, distinct from a genuinely not-found run
  ID (caught earlier, at the existence-check step above); any *other* phases-endpoint
  failure (an unexpected 404 despite the run existing, a 5xx, a network error) falls
  back to a basic status view (status/last-completed-phase/timing + pause controls, no
  phase rail) rather than a broken page — same shape the pre-redesign `RunDetail.tsx`
  always showed.
- The phase rail refetches `GET /api/runs/{runID}/phases` on every SSE update/done
  event (not on a fixed poll interval), but only the very first fetch shows a loading
  state; later refreshes swap in silently so the rail never flickers while a run is
  progressing.
- The run-config viewer (`ConfigSummary.tsx`, `GET /api/runs/{runID}/config`) is a
  sources list + physical-tapes/redundancy stat cards + a collapsible raw-JSON
  disclosure for everything else (library device paths, encryption recipients,
  delivery/optical-burn settings) — not a bespoke renderer for every config field.
  `SubmitRunForm.tsx` is itself still JSON-only pending a later Config-page redesign; a
  fuller structured config viewer belongs there, not here. "Physical tapes" is computed
  from the Pack phase's own observed `logicalTapes`/`copies` facts (`GET
  /api/runs/{runID}/phases`), not re-derived from the submitted config, since the
  *packed* plan is the actually-observed result (SPEC §4.2); it reads as an em dash
  before Pack completes.
- `LogPanel.tsx`, `DriveMetricsPanel.tsx`, and `PauseActions.tsx` are reused
  unmodified, just re-homed into the new layout (per-phase log panels,
  `DriveMetricsPanel` embedded only in the Write phase's own view, `PauseActions`
  inside `RunOverview.tsx`'s operator-pause zone) — none of their existing
  kind-generic pause handling, unavailable-state handling, or terminal/live switching
  needed to change for the redesign.
- The config page validates client-side against the *committed* schema, served
  verbatim by a new `GET /api/config/schema` (a new `schemas` Go package embeds
  `schemas/run-config.schema.json`), interpreted by a small hand-written
  JSON-Schema-subset validator (`web/src/configSchema.ts`) covering exactly the
  draft-2020-12 features the committed schema uses — no ajv-style dependency, matching
  the hand-rolled-router/hand-drawn-icons precedent. Cross-field "exactly one of"
  invariants (Source `zfsPath`/`k8s`, Redundancy `targetPercentage`/`fillToCapacity`,
  K8sRef `name`/`labelSelector`) are not in the committed schema and are not
  re-implemented client-side: Form mode's single-choice toggles make violating them
  unrepresentable, and JSON mode defers to the server's `internal/config.Parse` as
  before.
- age keygen is server-side (`POST /api/age/keygen` wrapping the new
  `pkg/agewrap.GenerateIdentity`, `age-keygen -pq` — post-quantum only, per SPEC §7),
  not client-side WASM: it reuses the exact pinned binary the recovery disc ships. The
  recipient is re-derived from the generated identity via the existing
  `RecipientFromIdentity` (`age-keygen -y`), never parsed from keygen's comment output.
  The identity exists only in the single response; nothing server-side logs or stores
  it (unit-tested via a captured slog handler), and the UI shows it once with a copy
  control and warning.
- The Library section's blank-slot editor is a free-form add/remove list of slot
  numbers, not the design mock's fixed 44-button slot-chip grid — the mock's grid
  hardcodes one physical library's layout, while the schema's `blankSlots` is an
  arbitrary `[]integer`. The tape-capacity `select` uses real native capacities (LTO-6
  **2.5 TB**, correcting the mock's 2.4 TB error, cross-checked against
  `workflows/backup/report.go`'s generation thresholds), LTO-7 6 / LTO-8 12 /
  LTO-9 18 TB.
- Mode-switch semantics — Form → JSON always serializes current form state into the
  textarea; JSON → Form parses the text and populates the form when it parses as an
  object, otherwise keeps the form's previous state and says so. Neither mode's state
  is cleared by switching away. Advanced fields with no form control
  (`feasibilityOverhead`, the three operator-wait timeout overrides) are JSON-mode-only;
  Form mode omits them so the run gets `internal/config`'s defaults.
- The Review step's summary shows only client-side-knowable facts (source
  labels/counts, copies, redundancy policy, recipient count, disc copies) — never a
  predicted physical-tape count, which only the Pack phase's measured bin-packing can
  know (the mock's "6 physical tapes" is a frontend-invented figure).
- The config page's blocked state (run already active) reuses `useActiveRun`'s one-shot
  `GET /api/runs` check (the same sidebar mechanism) — still not a live subscription;
  the server-side 409 remains the authoritative guard.
- Mid-session 401 handling is client-side and centralized in the SPA's shared fetch
  helper (`web/src/api.ts`), the one choke point every `/api/*` request already flows
  through: a 401 response fires a tiny module-level `onSessionExpired` pub-sub, whose
  sole subscriber (`identity.ts`'s `useIdentity`) flips the identity state to
  `unauthenticated` — `AuthGate`'s existing effect then routes to `/login?redirect=`
  exactly as it already did for a 401 at first load, and `pkg/webauth`'s callback
  already 302s back to that path after sign-in. No new routes, contexts, or server
  changes. A single 401 is treated as authoritative session loss with no
  probe/debounce/confirmation step, because `pkg/webauth` emits 401 only for a
  genuinely absent/invalid session; the flaky-proxy worry maps to network-level
  failures (fetch rejecting) or non-401 statuses, which deliberately do NOT trigger
  this path and keep rendering as each component's in-place error. SSE is the one blind
  spot (EventSource surfaces no HTTP status), so `useRunEvents`'s error handler adds a
  follow-up `GET /api/me` probe (deduped to one in flight): if the stream died because
  the session did, the probe's own 401 cascades through the same pub-sub; any other
  probe outcome leaves EventSource's native auto-reconnect alone.
- The guided Form mode's host-specific fields — the library changer/drive devices and
  the Discord webhook — are deploy-owned, not per-run: they come from `cmd/web`'s
  `LIBRARY_CHANGER`/`LIBRARY_DRIVES`/`DELIVERY_WEBHOOK_URL`, surfaced by extending the
  existing `GET /api/config/ui` route (the same deploy-config surface the Temporal-UI
  link uses, nested under a `library` object so the companion library-topology work can
  extend it further). Deploy-owned fields are enforced **server-side**: `pkg/runsapi`'s
  `submitRun` calls `applyDeployConfig` to overwrite each configured deploy-owned field
  onto every submitted run config before it reaches Temporal, so a client (JSON / paste
  mode, or a direct `POST /api/runs`) cannot target a changer, drive, or webhook the
  host does not own. The overwrite runs *before* the dry-run mhvtl redirect
  (`runsubmit.ApplyDryRun`), so a dry run still lands on the virtual library and never
  touches real hardware; a field the deployment left unset is not overwritten, so it
  can still be supplied per run (and Review reports the validation error if it is
  missing). With the server authoritative, Form mode shows no control for these fields
  at all (only the slot-grid picker remains deploy-driven in the form). A JSON → Form
  switch that carries device/webhook values shows a notice that Form mode drops them in
  favor of deploy config — the same nothing-silently-discarded contract as the
  advanced-fields (`unmodeledFields`) notice.
