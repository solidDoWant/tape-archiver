import type { ReactNode } from 'react'
import { useUiConfig, type UiConfig } from './uiConfig'

// Row is one fact line — omitted by the caller entirely (never rendered
// blank) when its value is empty, per issue #276 AC6.
function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between gap-4">
      <span>{label}</span>
      <span className="text-text break-all">{value}</span>
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

// HardwareEnvCard is the dashboard's persistent "Hardware & environment" card
// (issue #276 AC6, #316 Part A). It describes the deployment's fixed
// environment — the library/burner device targets, delivery webhook, physical
// topology, and Temporal coordinates — sourced from the deploy-config endpoint
// (GET /api/config/ui via useUiConfig), never from a specific run's submitted
// config. Reading it from a run was wrong (issue #318): a run's config carries
// per-run values (e.g. encryption recipients, which have no deploy source and
// are dropped here), and runsubmit.ApplyDryRun rewrites its changer/drives with
// mhvtl virtual nodes, so a dry-run would make the card report virtual devices.
// Sourcing from deploy config makes the card correct before the first run is
// ever submitted and immune to dry-run overrides.
//
// A value that is unset in the deploy config is simply omitted (its row does
// not render at all) rather than shown blank or as a placeholder. The one
// exception is the delivery webhook, which always renders as "Configured" /
// "Not configured": the URL is a credential and is never shown, so the
// configured/not state is itself the value.
function HardwareEnvCard() {
  const state = useUiConfig()

  return (
    <CardShell>
      {state.status === 'loading' ? (
        <p role="status" className="text-[12.5px] text-text-faint">
          Loading…
        </p>
      ) : null}

      {state.status === 'error' ? (
        <p className="text-[12.5px] text-text-faint">Not reported — deploy config unavailable.</p>
      ) : null}

      {state.status === 'loaded' ? <HardwareEnvFacts config={state.config} /> : null}
    </CardShell>
  )
}

function HardwareEnvFacts({ config }: { config: UiConfig }) {
  const { library, delivery, temporalUiBaseUrl, temporalNamespace } = config

  const drives = library.drives.join(' · ')
  const burnerDrives = delivery.opticalBurnDrives.join(' · ')
  const cleaningSlots = library.cleaningSlots.join(', ')
  const ioStationSlots = library.ioStationSlots.join(', ')

  return (
    <div className="flex flex-col gap-0.5 font-mono text-[11px] text-text-dim">
      {library.changer ? <Row label="Changer" value={library.changer} /> : null}
      {drives ? <Row label="Drives" value={drives} /> : null}
      {burnerDrives ? <Row label="Burner drives" value={burnerDrives} /> : null}
      <Row label="Delivery webhook" value={delivery.webhookUrl ? 'Configured' : 'Not configured'} />
      {library.slotCount > 0 ? <Row label="Storage slots" value={String(library.slotCount)} /> : null}
      {cleaningSlots ? <Row label="Cleaning slots" value={cleaningSlots} /> : null}
      {ioStationSlots ? <Row label="I/O-station slots" value={ioStationSlots} /> : null}
      {temporalUiBaseUrl ? <Row label="Temporal UI" value={temporalUiBaseUrl} /> : null}
      {temporalNamespace ? <Row label="Temporal namespace" value={temporalNamespace} /> : null}
    </div>
  )
}

export default HardwareEnvCard
