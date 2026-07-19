import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import ErrorBoundary from './ErrorBoundary'

function Boom({ message }: { message: string }): never {
  throw new Error(message)
}

describe('ErrorBoundary', () => {
  beforeEach(() => {
    // React logs the caught error to console.error; silence it so the expected
    // throw does not look like a test failure in the output.
    vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('renders its children when they do not throw', () => {
    render(
      <ErrorBoundary>
        <div>all good</div>
      </ErrorBoundary>,
    )

    expect(screen.getByText('all good')).toBeInTheDocument()
  })

  it('renders a recoverable fallback instead of unmounting when a child throws', () => {
    render(
      <ErrorBoundary>
        <Boom message="sources.map is not a function" />
      </ErrorBoundary>,
    )

    expect(screen.getByRole('alert')).toBeInTheDocument()
    expect(screen.getByText(/Something went wrong rendering this page/)).toBeInTheDocument()
    // The underlying error message is surfaced for support.
    expect(screen.getByText('sources.map is not a function')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument()
  })

  it('names the scope from the label prop', () => {
    render(
      <ErrorBoundary label="the app">
        <Boom message="fatal" />
      </ErrorBoundary>,
    )

    expect(screen.getByText(/Something went wrong rendering the app/)).toBeInTheDocument()
  })
})
