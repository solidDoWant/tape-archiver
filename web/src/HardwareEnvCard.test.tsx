import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import HardwareEnvCard from './HardwareEnvCard'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

function stubFetch(status: number, body: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(jsonResponse(status, body))),
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('HardwareEnvCard', () => {
  it('shows "not reported" when no run has ever been submitted', () => {
    render(<HardwareEnvCard runId={null} />)

    expect(screen.getByText(/not reported/i)).toHaveTextContent(/no run has been submitted yet/i)
  })

  it('shows a loading state while fetching the run config', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})))

    render(<HardwareEnvCard runId="run-1" />)

    expect(screen.getByRole('status')).toHaveTextContent(/loading/i)
  })

  it('shows "not reported" with the reason when the config fetch fails (e.g. history aged out)', async () => {
    stubFetch(410, { error: 'run "run-1" exists but its Temporal workflow history has aged out' })

    render(<HardwareEnvCard runId="run-1" />)

    await waitFor(() => {
      expect(screen.getByText(/not reported/i)).toHaveTextContent(/aged out/i)
    })
  })

  it('shows only the configured values, omitting unset ones, never a hardcoded placeholder', async () => {
    stubFetch(200, {
      runId: 'run-1',
      config: {
        library: { changer: '/dev/sch0', drives: ['/dev/nst0', '/dev/nst1'] },
        delivery: { webhookUrl: '', opticalBurn: undefined },
        encryption: { recipients: [] },
      },
    })

    render(<HardwareEnvCard runId="run-1" />)

    await waitFor(() => {
      expect(screen.getByText('/dev/sch0')).toBeInTheDocument()
    })
    expect(screen.getByText('/dev/nst0 · /dev/nst1')).toBeInTheDocument()

    // Unset values never render a blank row or a design-sample placeholder.
    expect(screen.queryByText(/burner drives/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/delivery webhook/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/encryption recipient/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/discord\.com/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/age1pqxazr8w9/i)).not.toBeInTheDocument()
  })

  it('shows the delivery webhook as configured (redacted) and the full encryption recipient text when set', async () => {
    stubFetch(200, {
      runId: 'run-1',
      config: {
        library: { changer: '', drives: [] },
        delivery: { webhookUrl: '***redacted***', opticalBurn: { drives: ['/dev/sr0'] } },
        encryption: { recipients: ['age1exampleexampleexampleexampleexampleexampleexample'] },
      },
    })

    render(<HardwareEnvCard runId="run-1" />)

    await waitFor(() => {
      expect(screen.getByText('***redacted***')).toBeInTheDocument()
    })
    expect(screen.getByText('/dev/sr0')).toBeInTheDocument()
    expect(screen.getByText('age1exampleexampleexampleexampleexampleexampleexample')).toBeInTheDocument()

    // No changer/drives configured for this run — omitted, not blank.
    expect(screen.queryByText(/^changer$/i)).not.toBeInTheDocument()
  })
})
