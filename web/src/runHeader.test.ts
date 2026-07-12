import { describe, expect, it } from 'vitest'
import { headerRuntime, runStatusView } from './runHeader'

describe('runStatusView', () => {
  it('maps a running run to the RUNNING label and running tone', () => {
    const view = runStatusView('Running', false, false)

    expect(view.label).toBe('RUNNING')
    expect(view.title).toBe('Backup in progress')
    expect(view.tone).toBe('running')
    expect(view.badgeClass).toBe('text-blue')
  })

  it('reports a confirmed pause as PAUSED regardless of the underlying status', () => {
    const view = runStatusView('Running', true, false)

    expect(view.label).toBe('PAUSED')
    expect(view.tone).toBe('paused')
  })

  it('states the uncertainty (not "paused") when the pause query failed', () => {
    const view = runStatusView('Running', false, true)

    expect(view.label).toBe('PAUSE STATUS UNKNOWN')
    expect(view.tone).toBe('paused')
  })

  it('maps a completed run to COMPLETE/green and a failed run to FAILED/red', () => {
    expect(runStatusView('Completed', false, false)).toMatchObject({ label: 'COMPLETE', tone: 'complete' })
    expect(runStatusView('Failed', false, false)).toMatchObject({ label: 'FAILED', tone: 'failed' })
    expect(runStatusView('Terminated', false, false)).toMatchObject({ tone: 'failed' })
    expect(runStatusView('Canceled', false, false)).toMatchObject({ label: 'CANCELED', tone: 'neutral' })
  })
})

describe('headerRuntime', () => {
  const start = '2026-07-09T12:00:00Z'
  const close = '2026-07-09T17:14:00Z'

  it('phrases a still-running run as "started … ago"', () => {
    expect(headerRuntime(start, undefined, false, false)).toMatch(/^started .+ ago$/)
  })

  it('phrases a paused run as "paused · … in"', () => {
    expect(headerRuntime(start, undefined, true, false)).toMatch(/^paused · .+ in$/)
  })

  it('phrases a closed run as "ran <duration>"', () => {
    expect(headerRuntime(start, close, false, true)).toBe('ran 5h 14m')
  })

  it('is empty when there is no start time to measure from', () => {
    expect(headerRuntime(undefined, undefined, false, false)).toBe('')
  })
})
