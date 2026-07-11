import { useEffect, useState, type ReactNode } from 'react'
import { apiFetch, ApiError, describeNetworkError } from './api'

// RunConfig is the subset of internal/config.Config this card reads, as
// returned by GET /api/runs/{runID}/config (pkg/runsapi/config.go) — the
// exact configuration originally submitted for that run. Only the
// deploy/hardware-shaped fields this card displays are declared; everything
// else in the real config (sources, redundancy, ...) is ignored here.
interface RunConfig {
  library: {
    changer: string
    drives: string[]
  }
  delivery: {
    webhookUrl: string
    opticalBurn?: {
      drives: string[]
    }
  }
  encryption: {
    recipients: string[]
  }
}

interface RunConfigResponse {
  runId: string
  config: RunConfig
}

type LoadState = { status: 'loading' } | { status: 'unavailable'; reason: string } | { status: 'loaded'; config: RunConfig }

export interface HardwareEnvCardProps {
  // runId is the current/most recently submitted run's ID — the same run
  // Dashboard.tsx's CurrentRunCard summarizes — since this card's values are
  // deploy-time config, sourced from an actual submitted run rather than
  // hardcoded (DESIGN_ANALYSIS.md §4). Null when no run has ever been
  // submitted, which this card reports as "not reported" rather than a
  // silent blank.
  runId: string | null
}

// Row is one fact line — omitted by the caller entirely (never rendered
// blank) when its value is empty, per issue #276 AC6.
function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between gap-4">
      <span>{label}</span>
      <span className="text-text">{value}</span>
    </div>
  )
}

function CardShell({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-xl border border-border bg-surface p-4.5 shadow-card">
      <div className="mb-3.5 text-[12.5px] font-semibold">Hardware &amp; environment</div>
      {children}
    </div>
  )
}

// HardwareEnvCard is the dashboard's persistent "Hardware & environment"
// card (issue #276 AC6, DESIGN_ANALYSIS.md §2.A #4): changer/drive/burner
// device paths, the delivery webhook, and the age encryption recipient(s) —
// every value read from the current/last submitted run's own configuration
// (GET /api/runs/{runID}/config), never hardcoded. A value that is unset in
// that config is simply omitted (its row does not render at all) rather
// than shown blank or as a design-sample placeholder.
//
// Split into this thin wrapper plus HardwareEnvCardForRun (below) so "no run
// has ever been submitted" is a plain conditional render, never a fetch
// effect that would otherwise need to setState synchronously on its own
// skip path (the react-hooks/set-state-in-effect anti-pattern RunDetail.tsx's
// doc comment also calls out) — HardwareEnvCardForRun's effect always has a
// real run ID to fetch and never needs that branch at all.
function HardwareEnvCard({ runId }: HardwareEnvCardProps) {
  if (!runId) {
    return (
      <CardShell>
        <p className="text-[12.5px] text-text-faint">Not reported — no run has been submitted yet.</p>
      </CardShell>
    )
  }

  return <HardwareEnvCardForRun runId={runId} />
}

function HardwareEnvCardForRun({ runId }: { runId: string }) {
  const [state, setState] = useState<LoadState>({ status: 'loading' })

  useEffect(() => {
    let cancelled = false

    async function load() {
      try {
        const response = await apiFetch<RunConfigResponse>(`/api/runs/${encodeURIComponent(runId)}/config`)
        if (!cancelled) {
          setState({ status: 'loaded', config: response.config })
        }
      } catch (error) {
        if (!cancelled) {
          const message = error instanceof ApiError ? error.message : describeNetworkError(error)
          setState({ status: 'unavailable', reason: message })
        }
      }
    }

    void load()

    return () => {
      cancelled = true
    }
  }, [runId])

  return (
    <CardShell>
      {state.status === 'loading' ? (
        <p role="status" className="text-[12.5px] text-text-faint">
          Loading…
        </p>
      ) : null}

      {state.status === 'unavailable' ? (
        <p className="text-[12.5px] text-text-faint">Not reported — {state.reason}</p>
      ) : null}

      {state.status === 'loaded' ? <HardwareEnvFacts config={state.config} /> : null}
    </CardShell>
  )
}

function HardwareEnvFacts({ config }: { config: RunConfig }) {
  const drives = config.library.drives?.join(' · ') ?? ''
  const burnerDrives = config.delivery.opticalBurn?.drives?.join(' · ') ?? ''
  const recipients = config.encryption.recipients ?? []

  const anyDeviceRow = config.library.changer || drives || burnerDrives || config.delivery.webhookUrl

  return (
    <div className="flex flex-col gap-3.5">
      {anyDeviceRow ? (
        <div className="flex flex-col gap-0.5 font-mono text-[11px] text-text-dim">
          {config.library.changer ? <Row label="Changer" value={config.library.changer} /> : null}
          {drives ? <Row label="Drives" value={drives} /> : null}
          {burnerDrives ? <Row label="Burner drives" value={burnerDrives} /> : null}
          {config.delivery.webhookUrl ? <Row label="Delivery webhook" value={config.delivery.webhookUrl} /> : null}
        </div>
      ) : null}

      {recipients.length > 0 ? (
        <div className="font-mono text-[11px] text-text-dim">
          <div>Encryption recipient{recipients.length === 1 ? '' : 's'}</div>
          {recipients.map((recipient) => (
            <div key={recipient} className="mt-1 leading-relaxed text-text break-all">
              {recipient}
            </div>
          ))}
        </div>
      ) : null}

      {!anyDeviceRow && recipients.length === 0 ? (
        <p className="text-[12.5px] text-text-faint">Not reported — this run&apos;s config has no values to show.</p>
      ) : null}
    </div>
  )
}

export default HardwareEnvCard
