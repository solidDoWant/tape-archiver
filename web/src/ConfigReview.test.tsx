import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import ConfigReview from './ConfigReview'
import { buildConfig, defaultFormState } from './configModel'

describe('ConfigReview', () => {
  it('renders the mode, summary fields, and the full config JSON', () => {
    const form = defaultFormState()
    form.sources[0].zfsName = 'bulk-pool-01/photos'
    form.sources[0].label = 'photos'
    form.recipients = ['age1pq1abc']

    const config = buildConfig(form)

    render(<ConfigReview config={config} dryRun={true} />)

    expect(screen.getByText('Dry-run (mhvtl)')).toBeInTheDocument()
    expect(screen.getByText(/1 · photos/)).toBeInTheDocument()
    expect(screen.getByText('2')).toBeInTheDocument() // copies
    expect(screen.getByText(/PAR2 fixed 10%/)).toBeInTheDocument()
    expect(screen.getByText(/age · 1 recipient\(s\)/)).toBeInTheDocument()
    expect(screen.getByText('off')).toBeInTheDocument() // recovery discs

    const jsonBlock = document.getElementById('config-review-json')
    expect(jsonBlock).toHaveTextContent('"bulk-pool-01/photos"')
    expect(jsonBlock).toHaveTextContent('"age1pq1abc"')
  })

  it('shows Production mode and recovery-disc copies when set', () => {
    const form = defaultFormState()
    form.opticalBurnEnabled = true
    form.opticalCopies = 3

    render(<ConfigReview config={buildConfig(form)} dryRun={false} />)

    expect(screen.getByText('Production')).toBeInTheDocument()
    expect(screen.getByText(/on · 3 copies/)).toBeInTheDocument()
  })

  it('renders fill-to-capacity redundancy distinctly from a fixed percentage', () => {
    const form = defaultFormState()
    form.redundancyMode = 'fillToCapacity'
    form.fillFloor = 5

    render(<ConfigReview config={buildConfig(form)} dryRun={false} />)

    expect(screen.getByText(/fill to capacity · floor 5%/)).toBeInTheDocument()
  })
})
