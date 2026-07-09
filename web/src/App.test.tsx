import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import App from './App'

describe('App', () => {
  it('renders the shell heading and the submit-run form', () => {
    render(<App />)

    expect(
      screen.getByRole('heading', { name: 'tape-archiver' }),
    ).toBeInTheDocument()
    expect(screen.getByRole('form', { name: /submit backup run/i })).toBeInTheDocument()
  })
})
