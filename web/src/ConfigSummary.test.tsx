import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import ConfigSummary from './ConfigSummary'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('ConfigSummary', () => {
  it('renders sources, physical-tape/redundancy stats, and a raw-JSON disclosure once loaded', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, {
          runId: 'run-1',
          config: {
            sources: [
              { zfsPath: { name: 'bulk-pool-01/photos' }, compression: true },
              { zfsPath: { name: 'bulk-pool-01/vault' }, compression: false },
              { k8s: { kind: 'VolumeSnapshot', namespace: 'media', name: 'media-pvc' } },
            ],
            copies: 2,
            redundancy: { targetPercentage: 10 },
          },
        }),
      ),
    )

    render(<ConfigSummary runId="run-1" logicalTapes={3} copies={2} />)

    await waitFor(() => {
      expect(screen.getByText('bulk-pool-01/photos')).toBeInTheDocument()
    })
    expect(screen.getByText('bulk-pool-01/vault')).toBeInTheDocument()
    expect(screen.getByText('k8s · VolumeSnapshot/media-pvc (media)')).toBeInTheDocument()

    // Physical tapes = logicalTapes (3) × copies (2).
    expect(screen.getByText('6')).toBeInTheDocument()
    expect(screen.getByText(/3 logical × 2 copies/)).toBeInTheDocument()
    expect(screen.getByText('10')).toBeInTheDocument()

    // Compression badges: zstd (default/true) vs raw (explicit false).
    expect(screen.getAllByText('zstd')).toHaveLength(2) // photos + the k8s source (unset ⇒ on).
    expect(screen.getByText('raw')).toBeInTheDocument()

    expect(screen.getByText(/view submitted configuration/i)).toBeInTheDocument()
  })

  it('shows an em dash for physical tapes before the Pack phase has run', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(200, { runId: 'run-1', config: { sources: [], copies: 2, redundancy: {} } }),
      ),
    )

    render(<ConfigSummary runId="run-1" />)

    await waitFor(() => {
      expect(screen.getByText(/not yet packed/i)).toBeInTheDocument()
    })
  })

  it('shows an unavailable notice when the config has aged out (410)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(410, { error: 'gone' })))

    render(<ConfigSummary runId="run-1" />)

    await waitFor(() => {
      expect(screen.getByText(/no longer available/i)).toBeInTheDocument()
    })
  })

  it('shows an error for an unexpected failure', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('network down')))

    render(<ConfigSummary runId="run-1" />)

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/could not reach the server/i)
    })
  })
})
