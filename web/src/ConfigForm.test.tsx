import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import ConfigForm from './ConfigForm'
import { buildConfig, defaultFormState, type DeployConfig, type FormState } from './configModel'
import type { UiConfigState } from './uiConfig'

// testDeploy is a fully-configured deployment (issue #304): the deploy-owned
// devices/webhook are not shown in the form at all, and buildConfig fills them
// into the submitted config.
const testDeploy: DeployConfig = {
  changer: '/dev/sch0',
  drives: ['/dev/nst0', '/dev/nst1'],
  webhookUrl: 'https://discord.com/api/webhooks/1/a',
  // The deploy-owned optical burner drives: enforced server-side and never
  // rendered in the form (like the changer/drives).
  opticalBurnDrives: ['/dev/sr0', '/dev/sr1'],
  // A small topology (issue #305): 6 storage slots, slot 5 reserved for cleaning
  // and slot 6 for the I/O station, so the slot-grid picker offers slots 1–4 as
  // selectable blank-tape targets and renders 5–6 non-selectable.
  slotCount: 6,
  cleaningSlots: [5],
  ioStationSlots: [6],
}

// emptyDeploy is a deployment that configured neither devices/webhook nor a
// library topology — every field empty. The Library/Delivery sections then show
// their "not configured" notices and the slot grid its "not configured" state.
const emptyDeploy: DeployConfig = {
  changer: '',
  drives: [],
  webhookUrl: '',
  opticalBurnDrives: [],
  slotCount: 0,
  cleaningSlots: [],
  ioStationSlots: [],
}

function Wrapper({
  onForm,
  deploy = testDeploy,
  deployStatus = 'loaded',
}: {
  onForm?: (form: FormState) => void
  deploy?: DeployConfig
  deployStatus?: UiConfigState['status']
}) {
  const [form, setForm] = useState(defaultFormState)
  onForm?.(form)

  return (
    <ConfigForm form={form} setForm={(updater) => setForm(updater)} deploy={deploy} deployStatus={deployStatus} />
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('ConfigForm', () => {
  it('starts with one ZFS source and lets the operator fill in its dataset name', () => {
    let latest: FormState | undefined
    render(<Wrapper onForm={(form) => (latest = form)} />)

    fireEvent.change(screen.getByPlaceholderText('bulk-pool-01/dataset'), {
      target: { value: 'bulk-pool-01/photos' },
    })

    expect(latest?.sources[0].zfsName).toBe('bulk-pool-01/photos')
    expect(buildConfig(latest as FormState, testDeploy).sources[0].zfsPath).toEqual({ name: 'bulk-pool-01/photos' })
  })

  it('switches a source to k8s and exposes the by-name fields', () => {
    let latest: FormState | undefined
    render(<Wrapper onForm={(form) => (latest = form)} />)

    fireEvent.click(within(screen.getByRole('group', { name: /source 1 type/i })).getByText('k8s'))

    expect(latest?.sources[0].type).toBe('k8s')
    expect(screen.getByPlaceholderText('media-pvc')).toBeInTheDocument()

    fireEvent.change(screen.getByPlaceholderText('media'), { target: { value: 'media-ns' } })
    fireEvent.change(screen.getByPlaceholderText('media-pvc'), { target: { value: 'media-pvc' } })

    const config = buildConfig(latest as FormState, testDeploy)
    expect(config.sources[0].k8s).toEqual({
      apiVersion: 'snapshot.storage.k8s.io/v1',
      kind: 'VolumeSnapshot',
      namespace: 'media-ns',
      name: 'media-pvc',
    })
  })

  it('adds and removes sources, never allowing the last one to be removed', () => {
    let latest: FormState | undefined
    render(<Wrapper onForm={(form) => (latest = form)} />)

    fireEvent.click(screen.getByRole('button', { name: /\+ add source/i }))
    expect(latest?.sources).toHaveLength(2)

    fireEvent.click(screen.getByRole('button', { name: /remove source 2/i }))
    expect(latest?.sources).toHaveLength(1)

    expect(screen.getByRole('button', { name: /remove source 1/i })).toBeDisabled()
  })

  it('toggles between fixed and fill-to-capacity redundancy modes', () => {
    let latest: FormState | undefined
    render(<Wrapper onForm={(form) => (latest = form)} />)

    expect(screen.getByLabelText('target %')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Fill to capacity' }))

    expect(latest?.redundancyMode).toBe('fillToCapacity')
    expect(screen.getByLabelText('floor %')).toBeInTheDocument()
    expect(screen.queryByLabelText('target %')).not.toBeInTheDocument()
  })

  it('renders a slot grid bounded to the deploy topology, excluding cleaning and I/O-station slots (issue #305)', () => {
    render(<Wrapper />)

    // Storage slots 1–4 are selectable buttons...
    expect(screen.getByRole('button', { name: 'Slot 1' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Slot 4' })).toBeInTheDocument()

    // ...the cleaning (5) and I/O-station (6) slots are rendered but not
    // selectable...
    expect(screen.queryByRole('button', { name: 'Slot 5' })).not.toBeInTheDocument()
    expect(screen.getByLabelText('Slot 5 — reserved for cleaning')).toBeInTheDocument()
    expect(screen.getByLabelText('Slot 6 — reserved for I/O-station')).toBeInTheDocument()

    // ...and the grid stops at the declared slot count (no slot 7).
    expect(screen.queryByRole('button', { name: 'Slot 7' })).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Slot 7 — reserved for cleaning')).not.toBeInTheDocument()
  })

  it('toggles a blank storage slot by clicking its grid cell', () => {
    let latest: FormState | undefined
    render(<Wrapper onForm={(form) => (latest = form)} />)

    fireEvent.click(screen.getByRole('button', { name: 'Slot 3' }))
    expect(latest?.blankSlots).toEqual([3])
    expect(screen.getByText('1 blank slot(s) selected')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Slot 3' })).toHaveAttribute('aria-pressed', 'true')

    // Clicking a selected slot deselects it.
    fireEvent.click(screen.getByRole('button', { name: 'Slot 3' }))
    expect(latest?.blankSlots).toEqual([])
    expect(screen.getByText('0 blank slot(s) selected')).toBeInTheDocument()
  })

  it('warns inline when the selected blank-slot count is not a multiple of copies, and clears the warning once it is', () => {
    render(<Wrapper />)

    // Default copies is 2. One blank slot cannot form a whole logical-tape copy
    // set, so the picker warns.
    fireEvent.click(screen.getByRole('button', { name: 'Slot 1' }))
    expect(screen.getByRole('alert')).toHaveTextContent(/not a multiple of 2 copies/i)

    // A second blank slot makes the count a whole multiple of copies — warning gone.
    fireEvent.click(screen.getByRole('button', { name: 'Slot 2' }))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('adapts the grid to a different slot count with no code change (issue #305)', () => {
    render(<Wrapper deploy={{ ...testDeploy, slotCount: 10, cleaningSlots: [], ioStationSlots: [] }} />)

    // Ten selectable storage slots, driven entirely by the deploy value.
    expect(screen.getAllByRole('button', { name: /^Slot \d+$/ })).toHaveLength(10)
  })

  it('shows a not-configured notice for the slot picker when the deployment declared no topology', () => {
    render(<Wrapper deploy={emptyDeploy} deployStatus="loaded" />)

    expect(screen.getByText(/set LIBRARY_SLOT_COUNT/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^Slot \d+$/ })).not.toBeInTheDocument()
  })

  it('reveals the optical-burn fields only once enabled', () => {
    render(<Wrapper />)

    expect(screen.queryByText(/copies per run/i)).not.toBeInTheDocument()

    fireEvent.click(screen.getByLabelText('Enable optical recovery discs'))

    expect(screen.getByText(/copies per run/i)).toBeInTheDocument()
  })

  it('does not render the deploy-owned burner drives in the form at all, even when optical burn is enabled', () => {
    render(<Wrapper />)

    fireEvent.click(screen.getByLabelText('Enable optical recovery discs'))

    // The burner drives are deploy-owned and enforced server-side (like the
    // changer/drive devices), so the form shows no control — not even a
    // read-only display — for them: neither the device paths nor any label.
    expect(screen.queryByText('/dev/sr0')).not.toBeInTheDocument()
    expect(screen.queryByText('/dev/sr1')).not.toBeInTheDocument()
    expect(screen.queryByText(/burner devices/i)).not.toBeInTheDocument()

    // The operator still controls the copies-per-run input.
    expect(screen.getByText(/copies per run/i)).toBeInTheDocument()
  })

  it('inserts a generated age keypair into the recipients list and identity field', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ recipient: 'age1pq1generated', identity: 'AGE-SECRET-KEY-PQ-1generated' }),
      }),
    )

    let latest: FormState | undefined
    render(<Wrapper onForm={(form) => (latest = form)} />)

    fireEvent.click(screen.getByRole('button', { name: /generate new age keypair/i }))

    await waitFor(() => {
      expect(latest?.identity).toBe('AGE-SECRET-KEY-PQ-1generated')
    })

    expect(latest?.recipients).toContain('age1pq1generated')
  })

  it('does not render the deploy-owned changer, drives, or webhook in the form at all (issue #304)', () => {
    render(<Wrapper />)

    // The deploy-owned devices/webhook are enforced server-side and filled into
    // the submitted config by buildConfig, so the form shows no control — not
    // even a read-only display — for them: neither the values nor any input.
    expect(screen.queryByText('/dev/sch0')).not.toBeInTheDocument()
    expect(screen.queryByText('/dev/nst0')).not.toBeInTheDocument()
    expect(screen.queryByText('/dev/nst1')).not.toBeInTheDocument()
    expect(screen.queryByText('https://discord.com/api/webhooks/1/a')).not.toBeInTheDocument()

    expect(screen.queryByText(/changer device/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/drive devices/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/Discord webhook/i)).not.toBeInTheDocument()

    expect(screen.queryByPlaceholderText('/dev/sch0')).not.toBeInTheDocument()
    expect(screen.queryByPlaceholderText('/dev/nst0')).not.toBeInTheDocument()
    expect(screen.queryByPlaceholderText('https://discord.com/api/webhooks/…')).not.toBeInTheDocument()

    // A deployment that configured none of them likewise shows no device/webhook
    // env-var hint in the form (only the slot-grid picker keeps its own notice).
    expect(screen.queryByText(/set LIBRARY_CHANGER/)).not.toBeInTheDocument()
    expect(screen.queryByText(/set LIBRARY_DRIVES/)).not.toBeInTheDocument()
    expect(screen.queryByText(/set DELIVERY_WEBHOOK_URL/)).not.toBeInTheDocument()
  })
})
