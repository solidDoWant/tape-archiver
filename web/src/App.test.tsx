import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import App from './App'

describe('App', () => {
  it('renders the tape-archiver placeholder shell', () => {
    render(<App />)

    expect(
      screen.getByRole('heading', { name: 'tape-archiver' }),
    ).toBeInTheDocument()
    expect(screen.getByText(/under construction/i)).toBeInTheDocument()
  })
})
