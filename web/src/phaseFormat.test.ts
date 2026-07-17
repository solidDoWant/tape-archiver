import { afterEach, describe, expect, it, vi } from 'vitest'
import { formatDuration, phaseLabel } from './phaseFormat'

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
