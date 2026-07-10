import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import NotFoundPage from './NotFoundPage'
import { RouterProvider } from './router'

beforeEach(() => {
  window.history.pushState({}, '', '/no-such-page')
})

afterEach(() => {
  window.history.pushState({}, '', '/')
})

describe('NotFoundPage', () => {
  it('shows a 404 heading and the path that was not found', () => {
    render(
      <RouterProvider>
        <NotFoundPage path="/no-such-page" />
      </RouterProvider>,
    )

    expect(screen.getByText('404')).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: /page not found/i })).toBeInTheDocument()
    expect(screen.getByText('/no-such-page')).toBeInTheDocument()
  })

  it('offers a way back to the dashboard', () => {
    render(
      <RouterProvider>
        <NotFoundPage path="/no-such-page" />
      </RouterProvider>,
    )

    fireEvent.click(screen.getByRole('link', { name: /back to dashboard/i }))
    expect(window.location.pathname).toBe('/')
  })
})
