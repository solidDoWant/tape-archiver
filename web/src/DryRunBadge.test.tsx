import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import DryRunBadge from './DryRunBadge'

describe('DryRunBadge', () => {
  it('marks a dry-run', () => {
    render(<DryRunBadge dryRun={true} />)

    expect(screen.getByText('DRY-RUN')).toBeInTheDocument()
  })

  it('renders nothing for a production run', () => {
    const { container } = render(<DryRunBadge dryRun={false} />)

    expect(screen.queryByText('DRY-RUN')).not.toBeInTheDocument()
    expect(container).toBeEmptyDOMElement()
  })
})
