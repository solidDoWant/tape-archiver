import { describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import PhaseRail from './PhaseRail'
import { formatDuration, phaseLabel } from './phaseFormat'
import type { PhaseInfo } from './RunDetail'

const phases: PhaseInfo[] = [
  { name: 'Resolve', status: 'completed', startTime: '2026-07-09T12:00:00Z', endTime: '2026-07-09T12:01:00Z', facts: [] },
  { name: 'Prepare', status: 'completed', startTime: '2026-07-09T12:01:00Z', endTime: '2026-07-09T12:02:00Z', facts: [] },
  { name: 'Pack', status: 'active', startTime: '2026-07-09T12:02:00Z', facts: [] },
  { name: 'Generate PAR2', status: 'pending', facts: [] },
  { name: 'Verify', status: 'pending', facts: [] },
  { name: 'Load', status: 'pending', facts: [] },
  { name: 'Write', status: 'pending', facts: [] },
  { name: 'Eject', status: 'pending', facts: [] },
  { name: 'Report', status: 'pending', facts: [] },
  { name: 'Burn', status: 'pending', facts: [] },
  { name: 'Deliver', status: 'failed', error: 'boom', facts: [] },
]

describe('phaseLabel', () => {
  it('displays "Generate PAR2" as "PAR2"', () => {
    expect(phaseLabel('Generate PAR2')).toBe('PAR2')
  })

  it('passes every other phase name through unchanged', () => {
    expect(phaseLabel('Write')).toBe('Write')
  })
})

describe('formatDuration', () => {
  it('renders an em dash for a phase with no start time', () => {
    expect(formatDuration(undefined, undefined)).toBe('—')
  })

  it('renders seconds for a short span', () => {
    expect(formatDuration('2026-07-09T12:00:00Z', '2026-07-09T12:00:42Z')).toBe('42s')
  })

  it('renders minutes and seconds for a longer span', () => {
    expect(formatDuration('2026-07-09T12:00:00Z', '2026-07-09T12:02:05Z')).toBe('2m 5s')
  })

  it('renders hours and minutes for a very long span', () => {
    expect(formatDuration('2026-07-09T12:00:00Z', '2026-07-09T14:30:00Z')).toBe('2h 30m')
  })
})

describe('PhaseRail', () => {
  it('renders a "Run overview" item plus all 11 phases, in the given order', () => {
    render(<PhaseRail phases={phases} selected="overview" onSelect={vi.fn()} />)

    const nav = screen.getByRole('navigation', { name: /run phases/i })
    const buttons = within(nav).getAllByRole('button')

    expect(buttons).toHaveLength(12)
    expect(buttons[0]).toHaveTextContent('Run overview')
    expect(buttons[4]).toHaveTextContent('PAR2')
    expect(buttons[11]).toHaveTextContent('Deliver')
  })

  it('marks the selected item current for assistive tech', () => {
    render(<PhaseRail phases={phases} selected="Pack" onSelect={vi.fn()} />)

    expect(screen.getByRole('button', { name: /^pack/i })).toHaveAttribute('aria-current', 'true')
    expect(screen.getByRole('button', { name: /run overview/i })).not.toHaveAttribute('aria-current')
  })

  it('calls onSelect with the phase name when a row is clicked', () => {
    const onSelect = vi.fn()
    render(<PhaseRail phases={phases} selected="overview" onSelect={onSelect} />)

    screen.getByRole('button', { name: /^write/i }).click()

    expect(onSelect).toHaveBeenCalledWith('Write')
  })

  it('calls onSelect with "overview" when the overview item is clicked', () => {
    const onSelect = vi.fn()
    render(<PhaseRail phases={phases} selected="Write" onSelect={onSelect} />)

    screen.getByRole('button', { name: /run overview/i }).click()

    expect(onSelect).toHaveBeenCalledWith('overview')
  })

  it('exposes each phase’s status via data-status, for completed/active/failed/pending', () => {
    render(<PhaseRail phases={phases} selected="overview" onSelect={vi.fn()} />)

    expect(screen.getByRole('button', { name: /^resolve/i })).toHaveAttribute('data-status', 'completed')
    expect(screen.getByRole('button', { name: /^pack/i })).toHaveAttribute('data-status', 'active')
    expect(screen.getByRole('button', { name: /^generate par2|^par2/i })).toHaveAttribute('data-status', 'pending')
    expect(screen.getByRole('button', { name: /^deliver/i })).toHaveAttribute('data-status', 'failed')
  })
})
