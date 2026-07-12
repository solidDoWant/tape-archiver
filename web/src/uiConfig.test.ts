import { describe, expect, it } from 'vitest'
import { temporalWorkflowUrl } from './uiConfig'

describe('temporalWorkflowUrl', () => {
  it('returns null when no config is loaded', () => {
    expect(temporalWorkflowUrl(undefined, 'backup', 'run-1')).toBeNull()
  })

  it('returns null when no Temporal UI base URL is configured', () => {
    expect(temporalWorkflowUrl({ temporalUiBaseUrl: '', temporalNamespace: 'prod' }, 'backup', 'run-1')).toBeNull()
  })

  it('builds Temporal Web UI history deep-link path', () => {
    expect(
      temporalWorkflowUrl({ temporalUiBaseUrl: 'https://temporal.example.com', temporalNamespace: 'prod' }, 'backup', 'run-1'),
    ).toBe('https://temporal.example.com/namespaces/prod/workflows/backup/run-1/history')
  })

  it('strips trailing slashes from the base URL', () => {
    expect(
      temporalWorkflowUrl({ temporalUiBaseUrl: 'http://localhost:8233/', temporalNamespace: 'default' }, 'backup', 'run-1'),
    ).toBe('http://localhost:8233/namespaces/default/workflows/backup/run-1/history')
  })

  it('defaults the namespace segment to "default" when unset', () => {
    expect(
      temporalWorkflowUrl({ temporalUiBaseUrl: 'http://ui', temporalNamespace: '' }, 'backup', 'run-1'),
    ).toBe('http://ui/namespaces/default/workflows/backup/run-1/history')
  })

  it('URL-encodes the namespace, workflow id, and run id', () => {
    expect(
      temporalWorkflowUrl({ temporalUiBaseUrl: 'http://ui', temporalNamespace: 'ns/one' }, 'work flow', 'run/2'),
    ).toBe('http://ui/namespaces/ns%2Fone/workflows/work%20flow/run%2F2/history')
  })
})
