import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import AgeKeygenPanel from './AgeKeygenPanel'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('AgeKeygenPanel', () => {
  it('generates a keypair, shows it once, and reports it to onGenerated', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ recipient: 'age1pq1generated', identity: 'AGE-SECRET-KEY-PQ-1generated' }),
    })
    vi.stubGlobal('fetch', fetchMock)

    const onGenerated = vi.fn()
    render(<AgeKeygenPanel onGenerated={onGenerated} />)

    fireEvent.click(screen.getByRole('button', { name: /generate new age keypair/i }))

    await waitFor(() => {
      expect(screen.getByRole('status')).toBeInTheDocument()
    })

    expect(screen.getByText('age1pq1generated')).toBeInTheDocument()
    expect(screen.getByText('AGE-SECRET-KEY-PQ-1generated')).toBeInTheDocument()
    expect(onGenerated).toHaveBeenCalledWith({
      recipient: 'age1pq1generated',
      identity: 'AGE-SECRET-KEY-PQ-1generated',
    })
    expect(fetchMock).toHaveBeenCalledWith('/api/age/keygen', { method: 'POST' })
  })

  it('shows the server error and never calls onGenerated on failure', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({ error: 'generate age keypair: age-keygen not found' }),
    })
    vi.stubGlobal('fetch', fetchMock)

    const onGenerated = vi.fn()
    render(<AgeKeygenPanel onGenerated={onGenerated} />)

    fireEvent.click(screen.getByRole('button', { name: /generate new age keypair/i }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/age-keygen not found/i)
    })

    expect(onGenerated).not.toHaveBeenCalled()
  })

  it('replaces the previously shown keypair when generated again (no way back to the old one)', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ recipient: 'age1pq1first', identity: 'AGE-SECRET-KEY-PQ-1first' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ recipient: 'age1pq1second', identity: 'AGE-SECRET-KEY-PQ-1second' }),
      })
    vi.stubGlobal('fetch', fetchMock)

    const onGenerated = vi.fn()
    render(<AgeKeygenPanel onGenerated={onGenerated} />)

    fireEvent.click(screen.getByRole('button', { name: /generate new age keypair/i }))
    await waitFor(() => expect(screen.getByText('AGE-SECRET-KEY-PQ-1first')).toBeInTheDocument())

    fireEvent.click(screen.getByRole('button', { name: /generate new age keypair/i }))
    await waitFor(() => expect(screen.getByText('AGE-SECRET-KEY-PQ-1second')).toBeInTheDocument())

    expect(screen.queryByText('AGE-SECRET-KEY-PQ-1first')).not.toBeInTheDocument()
    expect(onGenerated).toHaveBeenCalledTimes(2)
  })
})
