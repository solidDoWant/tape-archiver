import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import ConfigForm from './ConfigForm'
import { buildConfig, defaultFormState, type FormState } from './configModel'

function Wrapper({ onForm }: { onForm?: (form: FormState) => void }) {
  const [form, setForm] = useState(defaultFormState)
  onForm?.(form)

  return <ConfigForm form={form} setForm={(updater) => setForm(updater)} />
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
    expect(buildConfig(latest as FormState).sources[0].zfsPath).toEqual({ name: 'bulk-pool-01/photos' })
  })

  it('switches a source to k8s and exposes the by-name fields', () => {
    let latest: FormState | undefined
    render(<Wrapper onForm={(form) => (latest = form)} />)

    fireEvent.click(within(screen.getByRole('group', { name: /source 1 type/i })).getByText('k8s'))

    expect(latest?.sources[0].type).toBe('k8s')
    expect(screen.getByPlaceholderText('media-pvc')).toBeInTheDocument()

    fireEvent.change(screen.getByPlaceholderText('media'), { target: { value: 'media-ns' } })
    fireEvent.change(screen.getByPlaceholderText('media-pvc'), { target: { value: 'media-pvc' } })

    const config = buildConfig(latest as FormState)
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

  it('adds a blank slot number via the slot editor', () => {
    let latest: FormState | undefined
    render(<Wrapper onForm={(form) => (latest = form)} />)

    fireEvent.change(screen.getByLabelText('New blank slot number'), { target: { value: '3' } })
    fireEvent.click(screen.getByRole('button', { name: /\+ add slot/i }))

    expect(latest?.blankSlots).toEqual([3])
    expect(screen.getByText('1 blank slot(s) configured')).toBeInTheDocument()
  })

  it('reveals the optical-burn fields only once enabled', () => {
    render(<Wrapper />)

    expect(screen.queryByText(/copies per run/i)).not.toBeInTheDocument()

    fireEvent.click(screen.getByLabelText('Enable optical recovery discs'))

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
})
