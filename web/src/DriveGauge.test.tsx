import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import DriveGauge from './DriveGauge'

describe('DriveGauge', () => {
  it('shows a loading placeholder', () => {
    render(<DriveGauge driveIndex={0} status="loading" />)

    expect(screen.getByText(/loading drive metrics/i)).toBeInTheDocument()
  })

  it('shows an unavailable placeholder', () => {
    render(<DriveGauge driveIndex={0} status="unavailable" />)

    expect(screen.getByText(/metrics unavailable/i)).toBeInTheDocument()
  })

  it('shows a no-data placeholder for a drive with no reading yet', () => {
    render(<DriveGauge driveIndex={1} barcode="TA0001L6" status="no-data" />)

    expect(screen.getByText(/not writing yet/i)).toBeInTheDocument()
    expect(screen.getByText(/TA0001L6/)).toBeInTheDocument()
  })

  it('shows a live reading with rate, floor, and repositions', () => {
    render(
      <DriveGauge
        driveIndex={0}
        barcode="TA0001L6"
        status="live"
        throughputMBps={142.5}
        floorMBps={50}
        floorKnown
        repositions={2}
        tapeAlertFlagCount={0}
        belowFloor={false}
      />,
    )

    expect(screen.getByText('143 MB/s')).toBeInTheDocument()
    expect(screen.getByText(/floor 50/)).toBeInTheDocument()
    expect(screen.getByText(/2 rehits/)).toBeInTheDocument()
    expect(screen.queryByText(/below speed-matching floor/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/tapealert/i)).not.toBeInTheDocument()
  })

  it('visibly indicates a below-floor reading, not just via color', () => {
    render(
      <DriveGauge
        driveIndex={0}
        barcode="TA0001L6"
        status="live"
        throughputMBps={30}
        floorMBps={50}
        floorKnown
        belowFloor
      />,
    )

    expect(screen.getByText(/below speed-matching floor/i)).toBeInTheDocument()
  })

  it('visibly indicates active TapeAlert flags', () => {
    render(
      <DriveGauge driveIndex={0} barcode="TA0001L6" status="live" throughputMBps={80} tapeAlertFlagCount={2} />,
    )

    expect(screen.getByText(/2 tapealert flags/i)).toBeInTheDocument()
  })

  it('renders one rehit singular correctly', () => {
    render(
      <DriveGauge driveIndex={0} barcode="TA0001L6" status="live" throughputMBps={80} repositions={1} />,
    )

    expect(screen.getByText(/1 rehit\b/)).toBeInTheDocument()
  })
})
