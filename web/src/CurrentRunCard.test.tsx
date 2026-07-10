import type { ComponentProps } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import CurrentRunCard from './CurrentRunCard'
import { RouterProvider } from './router'
import type { RunSummary } from './api'
import type { RunEventsState } from './runEvents'

beforeEach(() => {
  window.history.pushState({}, '', '/')
})

const notLive: RunEventsState = { state: 'connecting', detail: null }

function run(overrides: Partial<RunSummary> & { runId: string }): RunSummary {
  return {
    workflowId: 'backup',
    status: 'Running',
    startTime: '2026-07-01T00:00:00Z',
    ...overrides,
  }
}

function renderCard(props: Partial<ComponentProps<typeof CurrentRunCard>> = {}) {
  const onStartRun = vi.fn()

  render(
    <RouterProvider>
      <CurrentRunCard
        loadState="loaded"
        activeRun={null}
        mostRecentRun={null}
        live={notLive}
        onStartRun={onStartRun}
        {...props}
      />
    </RouterProvider>,
  )

  return { onStartRun }
}

describe('CurrentRunCard', () => {
  it('shows a loading state', () => {
    renderCard({ loadState: 'loading' })

    expect(screen.getByRole('status')).toHaveTextContent(/loading current run/i)
  })

  it('shows an error state', () => {
    renderCard({ loadState: 'error', error: 'boom' })

    expect(screen.getByRole('alert')).toHaveTextContent('boom')
  })

  it('shows a first-run empty state with a Start a run control when no runs exist at all', () => {
    const { onStartRun } = renderCard({ activeRun: null, mostRecentRun: null })

    expect(screen.getByText(/no runs yet/i)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /start a run/i }))
    expect(onStartRun).toHaveBeenCalledTimes(1)
  })

  it('shows an idle state summarizing the most recent run with a Start a run control', () => {
    const { onStartRun } = renderCard({
      activeRun: null,
      mostRecentRun: run({ runId: 'run-1', status: 'Completed', closeTime: '2026-07-01T02:00:00Z' }),
    })

    expect(screen.getByText(/no active run/i)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /open last run run-1/i })).toHaveAttribute('href', '/runs/run-1')

    fireEvent.click(screen.getByRole('button', { name: /start a run/i }))
    expect(onStartRun).toHaveBeenCalledTimes(1)
  })

  it('shows a live active state with status, phase, and a progress bar, plus an Open run link', () => {
    renderCard({
      activeRun: run({ runId: 'run-live' }),
      live: {
        state: 'live',
        detail: {
          workflowId: 'backup',
          runId: 'run-live',
          status: 'Running',
          startTime: '2026-07-01T00:00:00Z',
          lastCompletedPhase: 'Write',
          currentPause: { kind: '' },
        },
      },
    })

    expect(screen.getByText('run-live')).toBeInTheDocument()
    expect(screen.getByText('Write')).toBeInTheDocument()
    // Write is phase 7 of the 11-phase pipeline.
    expect(screen.getByText('Phase 7 of 11')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /open run/i })).toHaveAttribute('href', '/runs/run-live')
  })

  it('shows a paused state with a narrative and Resume/Abort controls', () => {
    renderCard({
      activeRun: run({ runId: 'run-live' }),
      live: {
        state: 'live',
        detail: {
          workflowId: 'backup',
          runId: 'run-live',
          status: 'Running',
          startTime: '2026-07-01T00:00:00Z',
          lastCompletedPhase: 'Load',
          currentPause: {
            kind: 'write-failure',
            phase: 'Write',
            affectedTapes: ['TA0001L6'],
            reloadSlots: [101],
            errorSummary: 'mkltfs: drive reported a hard write error',
            canAbort: true,
          },
        },
      },
    })

    expect(screen.getByText(/run paused — needs you/i)).toBeInTheDocument()
    expect(screen.getByText('PAUSED')).toBeInTheDocument()
    expect(screen.getByText(/TA0001L6/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^resume$/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^abort$/i })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /open run/i })).toHaveAttribute('href', '/runs/run-live')
  })
})
