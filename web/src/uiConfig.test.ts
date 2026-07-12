import { describe, expect, it } from 'vitest'
import { deployConfigFrom, temporalWorkflowUrl, type UiConfig, type UiConfigState } from './uiConfig'

// uiConfig builds a full UiConfig from just the Temporal fields the deep-link
// tests care about, defaulting the deploy-owned library/delivery sections
// (issue #304) so each test only states what it exercises.
function uiConfig(overrides: Partial<UiConfig>): UiConfig {
  return {
    temporalUiBaseUrl: '',
    temporalNamespace: '',
    library: { changer: '', drives: [] },
    delivery: { webhookUrl: '' },
    ...overrides,
  }
}

describe('temporalWorkflowUrl', () => {
  it('returns null when no config is loaded', () => {
    expect(temporalWorkflowUrl(undefined, 'backup', 'run-1')).toBeNull()
  })

  it('returns null when no Temporal UI base URL is configured', () => {
    expect(temporalWorkflowUrl(uiConfig({ temporalNamespace: 'prod' }), 'backup', 'run-1')).toBeNull()
  })

  it('builds Temporal Web UI history deep-link path', () => {
    expect(
      temporalWorkflowUrl(uiConfig({ temporalUiBaseUrl: 'https://temporal.example.com', temporalNamespace: 'prod' }), 'backup', 'run-1'),
    ).toBe('https://temporal.example.com/namespaces/prod/workflows/backup/run-1/history')
  })

  it('strips trailing slashes from the base URL', () => {
    expect(
      temporalWorkflowUrl(uiConfig({ temporalUiBaseUrl: 'http://localhost:8233/', temporalNamespace: 'default' }), 'backup', 'run-1'),
    ).toBe('http://localhost:8233/namespaces/default/workflows/backup/run-1/history')
  })

  it('defaults the namespace segment to "default" when unset', () => {
    expect(
      temporalWorkflowUrl(uiConfig({ temporalUiBaseUrl: 'http://ui', temporalNamespace: '' }), 'backup', 'run-1'),
    ).toBe('http://ui/namespaces/default/workflows/backup/run-1/history')
  })

  it('URL-encodes the namespace, workflow id, and run id', () => {
    expect(
      temporalWorkflowUrl(uiConfig({ temporalUiBaseUrl: 'http://ui', temporalNamespace: 'ns/one' }), 'work flow', 'run/2'),
    ).toBe('http://ui/namespaces/ns%2Fone/workflows/work%20flow/run%2F2/history')
  })
})

describe('deployConfigFrom', () => {
  it('extracts the deploy-owned library devices and webhook once loaded', () => {
    const state: UiConfigState = {
      status: 'loaded',
      config: uiConfig({
        library: { changer: '/dev/sch0', drives: ['/dev/nst0', '/dev/nst1'] },
        delivery: { webhookUrl: 'https://discord.example/webhook' },
      }),
    }

    expect(deployConfigFrom(state)).toEqual({
      changer: '/dev/sch0',
      drives: ['/dev/nst0', '/dev/nst1'],
      webhookUrl: 'https://discord.example/webhook',
    })
  })

  it('yields empty values while loading, so the form shows a loading state and Review surfaces validation', () => {
    expect(deployConfigFrom({ status: 'loading' })).toEqual({ changer: '', drives: [], webhookUrl: '' })
  })

  it('yields empty values on a failed fetch', () => {
    expect(deployConfigFrom({ status: 'error' })).toEqual({ changer: '', drives: [], webhookUrl: '' })
  })
})
