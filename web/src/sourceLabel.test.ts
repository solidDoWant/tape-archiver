import { describe, expect, it } from 'vitest'
import { sourceLabel } from './sourceLabel'

describe('sourceLabel', () => {
  it('prefers an explicit label', () => {
    expect(sourceLabel({ label: 'Photos', zfsPath: { name: 'pool/a' } })).toBe('Photos')
  })

  it('falls back to a zfs path name', () => {
    expect(sourceLabel({ zfsPath: { name: 'pool/a' } })).toBe('pool/a')
  })

  it('renders a k8s ref by name in detail mode', () => {
    expect(
      sourceLabel({ k8s: { kind: 'VolumeSnapshot', name: 'snap', namespace: 'media' } }, { detail: true }),
    ).toBe('k8s · VolumeSnapshot/snap (media)')
  })

  it('does not render a dangling "Kind/" for a name-less k8s ref in detail mode', () => {
    // A half-filled ref (no name and no labelSelector) must fall back to the
    // kind alone, never "VolumeSnapshot/".
    expect(sourceLabel({ k8s: { kind: 'VolumeSnapshot' } }, { detail: true })).toBe('k8s · VolumeSnapshot')
  })

  it('falls back to (unlabeled) when a detail-mode k8s ref has neither kind nor name', () => {
    expect(sourceLabel({ k8s: { kind: '' } }, { detail: true })).toBe('(unlabeled)')
  })

  it('falls back to (unlabeled) for an empty or malformed source', () => {
    expect(sourceLabel({})).toBe('(unlabeled)')
    expect(sourceLabel(null as unknown as Parameters<typeof sourceLabel>[0])).toBe('(unlabeled)')
  })
})
