import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import ConfigReview from './ConfigReview'
import { buildConfig, defaultFormState, type DeployConfig } from './configModel'

// testDeploy supplies the deploy-owned library devices + webhook (issue #304)
// buildConfig fills into the config under review.
const testDeploy: DeployConfig = {
  changer: '/dev/sch0',
  drives: ['/dev/nst0'],
  webhookUrl: '',
  opticalBurnDrives: [],
  slotCount: 0,
  cleaningSlots: [],
  ioStationSlots: [],
}

describe('ConfigReview', () => {
  it('renders the mode, summary fields, and the full config JSON', () => {
    const form = defaultFormState()
    form.sources[0].zfsName = 'bulk-pool-01/photos'
    form.sources[0].label = 'photos'
    form.recipients = ['age1pq1abc']

    const config = buildConfig(form, testDeploy)

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

  it('falls back to "(unlabeled)" for a source with an empty label and an empty-string name', () => {
    // A present-but-empty field must never render as a blank summary (issue: the
    // sources line showed an empty value). Construct such a source directly.
    const config = buildConfig(defaultFormState(), testDeploy)
    config.sources = [{ compression: true, k8s: { apiVersion: 'snapshot.storage.k8s.io/v1', kind: 'VolumeSnapshot', name: '' } }]

    render(<ConfigReview config={config} dryRun={false} />)

    expect(screen.getByText('1 · (unlabeled)')).toBeInTheDocument()
  })

  it('shows Production mode and recovery-disc copies when set', () => {
    const form = defaultFormState()
    form.opticalBurnEnabled = true
    form.opticalCopies = 3

    render(<ConfigReview config={buildConfig(form, testDeploy)} dryRun={false} />)

    expect(screen.getByText('Production')).toBeInTheDocument()
    expect(screen.getByText(/on · 3 copies/)).toBeInTheDocument()
  })

  it('renders fill-to-capacity redundancy distinctly from a fixed percentage', () => {
    const form = defaultFormState()
    form.redundancyMode = 'fillToCapacity'
    form.fillFloor = 5

    render(<ConfigReview config={buildConfig(form, testDeploy)} dryRun={false} />)

    expect(screen.getByText(/fill to capacity · floor 5%/)).toBeInTheDocument()
  })
})
