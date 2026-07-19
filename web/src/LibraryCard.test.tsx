import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import LibraryCard from './LibraryCard'

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

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn(() => new Promise(() => {}))) // pending by default
})

describe('LibraryCard', () => {
  it('always shows the "not live" disclosure, regardless of data state', () => {
    render(<LibraryCard />)

    expect(screen.getByText(/live drive and slot occupancy is not available/i)).toBeInTheDocument()
  })

  it('shows a loading state', () => {
    render(<LibraryCard />)

    expect(screen.getByRole('status')).toHaveTextContent(/loading tape history/i)
  })

  it('shows an error state on a fetch failure', async () => {
    stubFetch(500, { error: 'temporal unavailable' })

    render(<LibraryCard />)

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent('temporal unavailable')
    })
  })

  it('shows an empty state when history has no tape outcomes yet', async () => {
    stubFetch(200, { tapes: [] })

    render(<LibraryCard />)

    await waitFor(() => {
      expect(screen.getByText(/no tapes recorded in run history yet/i)).toBeInTheDocument()
    })
  })

  it('summarizes tape outcomes by result, derived from run history', async () => {
    stubFetch(200, {
      tapes: [
        { barcode: 'TA0001L6', result: 'written', runId: 'run-1', runStartTime: '2026-07-01T00:00:00Z' },
        { barcode: 'TA0002L6', result: 'written', runId: 'run-1', runStartTime: '2026-07-01T00:00:00Z' },
        { barcode: 'TA0003L6', result: 'failed', runId: 'run-1', runStartTime: '2026-07-01T00:00:00Z' },
        { barcode: 'TA0004L6', result: 'loaded', runId: 'run-2', runStartTime: '2026-07-02T00:00:00Z' },
      ],
    })

    render(<LibraryCard />)

    await waitFor(() => {
      expect(screen.getByText('WRITTEN')).toBeInTheDocument()
    })
    expect(screen.getByText('2')).toBeInTheDocument() // written
    expect(screen.getAllByText('1')).toHaveLength(2) // failed + in-progress
  })

  it('notes when some runs could not be reconstructed, without failing the whole card', async () => {
    stubFetch(200, {
      tapes: [{ barcode: 'TA0001L6', result: 'written', runId: 'run-1', runStartTime: '2026-07-01T00:00:00Z' }],
      runErrors: [{ runId: 'run-old', error: 'history aged out of retention' }],
    })

    render(<LibraryCard />)

    await waitFor(() => {
      expect(screen.getByText(/1 run could not be reconstructed/i)).toBeInTheDocument()
    })
  })
})
