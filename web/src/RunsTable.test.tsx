import type { ComponentProps } from 'react'
import { beforeEach, describe, expect, it } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/react'
import RunsTable from './RunsTable'
import { RouterProvider } from './router'
import type { RunSummary } from './api'

beforeEach(() => {
  window.history.pushState({}, '', '/')
})

function renderTable(props: Partial<ComponentProps<typeof RunsTable>>) {
  return render(
    <RouterProvider>
      <RunsTable loadState="loaded" runs={[]} {...props} />
    </RouterProvider>,
  )
}

function run(overrides: Partial<RunSummary> & { runId: string }): RunSummary {
  return {
    workflowId: 'backup',
    status: 'Completed',
    startTime: '2026-07-01T00:00:00Z',
    closeTime: '2026-07-01T02:00:00Z',
    ...overrides,
  }
}

describe('RunsTable', () => {
  it('shows a loading state', () => {
    renderTable({ loadState: 'loading' })

    expect(screen.getByRole('status')).toHaveTextContent(/loading runs/i)
  })

  it('shows an error state', () => {
    renderTable({ loadState: 'error', error: 'boom' })

    expect(screen.getByRole('alert')).toHaveTextContent('boom')
  })

  it('shows an empty state when there are no runs yet', () => {
    renderTable({ loadState: 'loaded', runs: [] })

    expect(screen.getByText(/no runs yet/i)).toBeInTheDocument()
  })

  it('lists runs with their started time, duration, result, and last phase', () => {
    renderTable({
      runs: [run({ runId: 'run-1', status: 'Completed' })],
    })

    const row = screen.getByRole('link', { name: 'run-1' })
    expect(within(row).getByText('Completed')).toBeInTheDocument()
    expect(within(row).getByText('2h 0m')).toBeInTheDocument()
    // No live phase supplied for this row (it isn't the active run).
    expect(within(row).getByText('—')).toBeInTheDocument()
    expect(row).toHaveAttribute('href', '/runs/run-1')
  })

  it('shows "Running" as the duration and the live last-completed-phase for the currently active run', () => {
    renderTable({
      runs: [run({ runId: 'run-live', status: 'Running', closeTime: undefined })],
      liveRunId: 'run-live',
      liveLastCompletedPhase: 'Write',
    })

    const row = screen.getByRole('link', { name: 'run-live' })
    expect(within(row).getAllByText('Running')).toHaveLength(2) // duration cell + result badge
    expect(within(row).getByText('Write')).toBeInTheDocument()
  })

  it('paginates 8 runs per page', () => {
    const runs = Array.from({ length: 10 }, (_, i) => run({ runId: `run-${i}` }))
    renderTable({ runs })

    expect(screen.getAllByRole('link', { name: /^run-/ })).toHaveLength(8)
    expect(screen.getByText('Page 1 of 2')).toBeInTheDocument()

    const prev = screen.getByRole('button', { name: /prev/i })
    const next = screen.getByRole('button', { name: /next/i })
    expect(prev).toBeDisabled()
    expect(next).not.toBeDisabled()

    fireEvent.click(next)

    expect(screen.getAllByRole('link', { name: /^run-/ })).toHaveLength(2)
    expect(screen.getByText('Page 2 of 2')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /next/i })).toBeDisabled()

    fireEvent.click(screen.getByRole('button', { name: /prev/i }))

    expect(screen.getAllByRole('link', { name: /^run-/ })).toHaveLength(8)
    expect(screen.getByText('Page 1 of 2')).toBeInTheDocument()
  })

  it('navigates to the run detail page when a row is clicked', () => {
    renderTable({ runs: [run({ runId: 'run-1' })] })

    fireEvent.click(screen.getByRole('link', { name: 'run-1' }))

    expect(window.location.pathname).toBe('/runs/run-1')
  })
})
