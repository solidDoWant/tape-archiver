import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import WriteRateSparkline from './WriteRateSparkline'

const points = [
  { time: '2026-07-10T00:00:00Z', value: 100 },
  { time: '2026-07-10T00:01:30Z', value: 120 },
  { time: '2026-07-10T00:03:00Z', value: 90 },
]

describe('WriteRateSparkline', () => {
  it('shows an unavailable placeholder', () => {
    render(<WriteRateSparkline status="unavailable" points={[]} />)

    expect(screen.getByText(/write-rate history unavailable/i)).toBeInTheDocument()
  })

  it('shows a loading placeholder', () => {
    render(<WriteRateSparkline status="loading" points={[]} />)

    expect(screen.getByText(/loading write-rate history/i)).toBeInTheDocument()
  })

  it('shows a no-data placeholder when there are no points yet', () => {
    render(<WriteRateSparkline status="no-data" points={[]} />)

    expect(screen.getByText(/no write-rate samples yet/i)).toBeInTheDocument()
  })

  it('shows a no-data placeholder for a live status with an empty series', () => {
    render(<WriteRateSparkline status="live" points={[]} />)

    expect(screen.getByText(/no write-rate samples yet/i)).toBeInTheDocument()
  })

  it('renders the latest value, floor caption, and one bar per point', () => {
    render(<WriteRateSparkline status="live" points={points} floorMBps={95} floorKnown />)

    expect(screen.getByText('90 MB/s')).toBeInTheDocument()
    expect(screen.getByText(/floor 95/)).toBeInTheDocument()

    const chart = screen.getByRole('img')
    expect(chart).toHaveAccessibleName(/100, 120, 90/)
  })
})
