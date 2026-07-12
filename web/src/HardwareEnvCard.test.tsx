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

// A fully-configured GET /api/config/ui body (pkg/runsapi/uiconfig.go shape).
// Individual tests override fields to exercise the omit-when-unset behavior.
function uiConfig(overrides: Record<string, unknown> = {}) {
  return {
    temporalUiBaseUrl: 'https://temporal.example',
    temporalNamespace: 'backups',
    library: {
      changer: '/dev/sch0',
      drives: ['/dev/nst0', '/dev/nst1'],
      slotCount: 24,
      cleaningSlots: [23, 24],
      ioStationSlots: [1],
    },
    delivery: { webhookUrl: 'https://discord.example/webhook/secret', opticalBurnDrives: ['/dev/sr0'] },
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('HardwareEnvCard', () => {
  it('shows a loading state while fetching the deploy config', () => {
    vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {})))

    render(<HardwareEnvCard />)

    expect(screen.getByRole('status')).toHaveTextContent(/loading/i)
  })

  it('shows "not reported" when the deploy-config fetch fails', async () => {
    stubFetch(500, { error: 'boom' })

    render(<HardwareEnvCard />)

    await waitFor(() => {
      expect(screen.getByText(/not reported/i)).toHaveTextContent(/deploy config unavailable/i)
    })
  })

  it('surfaces every /api/config/ui field, never the raw webhook URL or an encryption recipient', async () => {
    stubFetch(200, uiConfig())

    render(<HardwareEnvCard />)

    await waitFor(() => {
      expect(screen.getByText('/dev/sch0')).toBeInTheDocument()
    })
    expect(screen.getByText('/dev/nst0 · /dev/nst1')).toBeInTheDocument()
    expect(screen.getByText('/dev/sr0')).toBeInTheDocument()
    expect(screen.getByText('24')).toBeInTheDocument()
    expect(screen.getByText('23, 24')).toBeInTheDocument()
    expect(screen.getByText('1')).toBeInTheDocument()
    expect(screen.getByText('https://temporal.example')).toBeInTheDocument()
    expect(screen.getByText('backups')).toBeInTheDocument()

    // The webhook is a credential: shown as configured, never its URL value.
    expect(screen.getByText('Configured')).toBeInTheDocument()
    expect(screen.queryByText(/discord\.example/i)).not.toBeInTheDocument()

    // No deploy source for encryption recipients — dropped entirely (#318).
    expect(screen.queryByText(/encryption recipient/i)).not.toBeInTheDocument()
  })

  it('omits unset device/topology/Temporal rows and shows the webhook as not configured', async () => {
    stubFetch(
      200,
      uiConfig({
        temporalUiBaseUrl: '',
        temporalNamespace: '',
        library: { changer: '', drives: [], slotCount: 0, cleaningSlots: [], ioStationSlots: [] },
        delivery: { webhookUrl: '', opticalBurnDrives: [] },
      }),
    )

    render(<HardwareEnvCard />)

    await waitFor(() => {
      expect(screen.getByText('Not configured')).toBeInTheDocument()
    })

    // Unset values never render a blank row or a placeholder.
    expect(screen.queryByText(/^changer$/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/^drives$/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/burner drives/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/storage slots/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/cleaning slots/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/i\/o-station slots/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/temporal/i)).not.toBeInTheDocument()
  })
})
