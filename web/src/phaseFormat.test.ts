import { afterEach, describe, expect, it, vi } from 'vitest'
import { formatDuration, phaseLabel, stalledWait, waitingSummary } from './phaseFormat'

describe('phaseFormat.phaseLabel', () => {
  it('shortens Generate PAR2 to PAR2 and passes every other phase through', () => {
    expect(phaseLabel('Generate PAR2')).toBe('PAR2')
    expect(phaseLabel('Write')).toBe('Write')
  })
})

describe('phaseFormat.formatDuration', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  it('returns an em dash for a phase with no start time', () => {
    expect(formatDuration(undefined)).toBe('—')
  })

  it('renders the closed duration between an explicit start and end', () => {
    expect(formatDuration('2026-01-01T00:00:00Z', '2026-01-01T00:02:03Z')).toBe('2m 3s')
  })

  it('renders live elapsed until now for an active phase with no end', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-01T00:01:00Z'))

    expect(formatDuration('2026-01-01T00:00:00Z')).toBe('1m 0s')
  })

  it('clamps to 0s for an active phase whose start is slightly in the future (clock skew)', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'))

    // Start 3s ahead of the client clock: must read "0s", not the em dash
    // api.formatDuration renders for a negative elapsed.
    expect(formatDuration('2026-01-01T00:00:03Z')).toBe('0s')
  })
})

describe('phaseFormat.waitingSummary', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  const waiting = { activity: 'Load', taskQueue: 'data', since: '2026-01-01T00:00:00Z', stalled: true }
  const base = 'Load was scheduled 5m 0s ago on the “data” task queue, but no worker has picked it up.'

  it.each([
    { pollers: undefined, want: base },
    { pollers: 0, want: `${base} No workers are polling that queue.` },
    { pollers: 1, want: `${base} 1 worker is polling that queue.` },
    { pollers: 2, want: `${base} 2 workers are polling that queue.` },
  ])('describes the wait with pollers=$pollers', ({ pollers, want }) => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-01T00:05:00Z'))

    expect(waitingSummary({ ...waiting, pollers })).toBe(want)
  })
})

describe('phaseFormat.stalledWait', () => {
  it('finds only a phase whose wait is stalled', () => {
    const coldStart = { activity: 'Load', taskQueue: 'data', since: '2026-01-01T00:00:00Z', stalled: false }

    expect(stalledWait([{ name: 'Load', status: 'active', facts: [], waiting: coldStart }])).toBeUndefined()
    expect(stalledWait([{ name: 'Load', status: 'active', facts: [], waiting: { ...coldStart, stalled: true } }])?.name).toBe(
      'Load',
    )
  })
})
