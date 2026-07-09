import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import SubmitRunForm from './SubmitRunForm'

const validConfigJSON = JSON.stringify({
  sources: [{ zfsPath: { name: 'bulk-pool-01/archive@snap' } }],
  copies: 2,
  library: {
    changer: '/dev/sch0',
    drives: ['/dev/nst0', '/dev/nst1'],
    blankSlots: [1, 2],
    tapeCapacityBytes: 2500000000000,
  },
  redundancy: { targetPercentage: 10, sliceSizeBytes: 1073741824 },
  encryption: {
    recipients: ['age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4'],
    identity: 'AGE-SECRET-KEY-PQ-1EXAMPLEONLYNOTAREAL',
  },
  delivery: { webhookUrl: 'https://discord.com/api/webhooks/123/abc' },
})

function fillConfig(text: string) {
  fireEvent.change(screen.getByLabelText('Run config (JSON)'), {
    target: { value: text },
  })
}

function submit() {
  fireEvent.click(screen.getByRole('button', { name: /submit run/i }))
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('SubmitRunForm', () => {
  it('shows the run ID returned by the API on success', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 201,
      json: async () => ({ workflowId: 'backup', runId: 'run-abc-123' }),
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<SubmitRunForm />)

    fillConfig(validConfigJSON)
    submit()

    await waitFor(() => {
      expect(screen.getByRole('status')).toBeInTheDocument()
    })

    expect(screen.getByText('run-abc-123')).toBeInTheDocument()
    expect(screen.getByText('backup')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()

    // The dry-run checkbox defaults to unchecked, so the request must say so.
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/runs',
      expect.objectContaining({
        method: 'POST',
        body: expect.stringContaining('"dryRun":false'),
      }),
    )
  })

  it('sends dryRun:true when the checkbox is checked', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 201,
      json: async () => ({ workflowId: 'backup', runId: 'run-dry-1' }),
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<SubmitRunForm />)

    fillConfig(validConfigJSON)
    fireEvent.click(screen.getByLabelText(/dry-run/i))
    submit()

    await waitFor(() => {
      expect(screen.getByRole('status')).toBeInTheDocument()
    })

    expect(fetchMock).toHaveBeenCalledWith(
      '/api/runs',
      expect.objectContaining({
        body: expect.stringContaining('"dryRun":true'),
      }),
    )
  })

  it('shows the API error message on a validation failure', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 400,
      json: async () => ({ error: 'sources: at least one source is required' }),
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<SubmitRunForm />)

    fillConfig('{"copies": 2}')
    submit()

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })

    expect(
      screen.getByText('sources: at least one source is required'),
    ).toBeInTheDocument()
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('shows the API error message on a singleton conflict (409)', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({
        error: 'a backup run is already in progress (workflow ID "backup", run ID run-1)',
      }),
    })
    vi.stubGlobal('fetch', fetchMock)

    render(<SubmitRunForm />)

    fillConfig(validConfigJSON)
    submit()

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })

    expect(screen.getByText(/already in progress/i)).toBeInTheDocument()
  })

  it('rejects invalid JSON client-side without calling the API', async () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    render(<SubmitRunForm />)

    fillConfig('not valid json')
    submit()

    const alert = await screen.findByRole('alert')

    expect(alert).toHaveTextContent(/not valid JSON/i)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('shows a network-failure message when fetch itself rejects', async () => {
    const fetchMock = vi.fn().mockRejectedValue(new TypeError('network down'))
    vi.stubGlobal('fetch', fetchMock)

    render(<SubmitRunForm />)

    fillConfig(validConfigJSON)
    submit()

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })

    expect(screen.getByText(/could not reach the server/i)).toBeInTheDocument()
  })
})
