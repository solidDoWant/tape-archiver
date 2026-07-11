import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import ConfigPage from './ConfigPage'
import { RouterProvider } from './router'
import { resetConfigSchemaCache } from './configSchema'
import { testRunConfigSchema } from './testSchemaFixture'

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

// stubApi mirrors App.test.tsx's own helper (test scaffolding, not
// exportable product code — duplicated rather than shared for the same
// reason App.test.tsx's stubControllableMatchMedia gives). Every ConfigPage
// test needs /api/runs (useActiveRun's one-shot check) and, once a test
// reaches Form mode's Review step or JSON mode's live indicator,
// /api/config/schema.
function stubApi(overrides: Record<string, { status: number; body: unknown }> = {}) {
  const routes: Record<string, { status: number; body: unknown }> = {
    '/api/runs': { status: 200, body: { runs: [] } },
    '/api/config/schema': { status: 200, body: testRunConfigSchema },
    ...overrides,
  }

  const fetchMock = vi.fn((input: string, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : String(input)
    const method = init?.method ?? 'GET'
    const key = `${method} ${url.split('?')[0]}`
    const route = routes[key] ?? routes[url.split('?')[0]]

    if (!route) {
      return Promise.resolve(jsonResponse(404, { error: `no stub for ${key}` }))
    }

    return Promise.resolve(jsonResponse(route.status, route.body))
  })

  vi.stubGlobal('fetch', fetchMock)

  return fetchMock
}

function renderPage(overrides?: Record<string, { status: number; body: unknown }>, onViewRun?: (id: string) => void) {
  const fetchMock = stubApi(overrides)
  render(
    <RouterProvider>
      <ConfigPage onViewRun={onViewRun} />
    </RouterProvider>,
  )

  return fetchMock
}

async function fillMinimalValidForm() {
  fireEvent.change(screen.getByPlaceholderText('bulk-pool-01/dataset'), {
    target: { value: 'bulk-pool-01/photos' },
  })
  fireEvent.change(screen.getByPlaceholderText('age1pq1…'), {
    target: { value: 'age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4' },
  })
  fireEvent.change(screen.getByLabelText(/identity \/ private key/i), {
    target: { value: 'AGE-SECRET-KEY-PQ-1EXAMPLE' },
  })
  fireEvent.change(screen.getByPlaceholderText(/discord.com\/api\/webhooks/i), {
    target: { value: 'https://discord.com/api/webhooks/123/abc' },
  })
}

afterEach(() => {
  vi.unstubAllGlobals()
  resetConfigSchemaCache()
})

describe('ConfigPage', () => {
  it('shows a blocked state with a link to the active run instead of the editor', async () => {
    renderPage({
      '/api/runs': {
        status: 200,
        body: { runs: [{ workflowId: 'backup', runId: 'run-live', status: 'Running', startTime: '2026-07-01T00:00:00Z' }] },
      },
    })

    await waitFor(() => {
      expect(screen.getByText(/a run is already in progress/i)).toBeInTheDocument()
    })

    expect(screen.queryByRole('group', { name: /config input mode/i })).not.toBeInTheDocument()

    const link = screen.getByRole('link', { name: /open current run/i })
    expect(link).toHaveAttribute('href', '/runs/run-live')
  })

  it('renders the Form-mode editor by default when no run is active', async () => {
    renderPage()

    await waitFor(() => {
      expect(screen.getByRole('group', { name: /config input mode/i })).toBeInTheDocument()
    })

    expect(screen.getByText('STEP 1 · BUILD')).toBeInTheDocument()
    expect(screen.getByText('Sources')).toBeInTheDocument()
  })

  it('serializes the current form into JSON text when switching to JSON mode', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.change(screen.getByPlaceholderText('bulk-pool-01/dataset'), {
      target: { value: 'bulk-pool-01/switch-test' },
    })

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))

    const textarea = screen.getByLabelText('Run config (JSON)') as HTMLTextAreaElement
    expect(textarea.value).toContain('bulk-pool-01/switch-test')
  })

  it('loads valid JSON text into the form when switching from JSON to Form mode', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))

    const config = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/from-json' } }],
      copies: 3,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [], tapeCapacityBytes: 2500000000000 },
      redundancy: { targetPercentage: 10, sliceSizeBytes: 1 },
      encryption: { recipients: [], identity: '' },
      delivery: { webhookUrl: '' },
    }

    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: JSON.stringify(config) } })
    fireEvent.click(screen.getByRole('button', { name: 'Form' }))

    expect(screen.getByPlaceholderText('bulk-pool-01/dataset')).toHaveValue('bulk-pool-01/from-json')
  })

  it('names the fields Form mode would drop when switching JSON with advanced-only fields to Form mode', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))

    const config = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/advanced' } }],
      copies: 2,
      feasibilityOverhead: 1.1,
      library: {
        changer: '/dev/sch0',
        drives: ['/dev/nst0'],
        blankSlots: [],
        tapeCapacityBytes: 2500000000000,
        ioWaitTimeoutSeconds: 3600,
      },
      redundancy: { targetPercentage: 10, sliceSizeBytes: 1 },
      encryption: { recipients: ['age1pq1abc'], identity: 'AGE-SECRET-KEY-PQ-1x' },
      delivery: { webhookUrl: 'https://discord.com/api/webhooks/1/a' },
    }

    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: JSON.stringify(config) } })
    fireEvent.click(screen.getByRole('button', { name: 'Form' }))

    // The form still loads (the modeled fields all populate)…
    expect(screen.getByPlaceholderText('bulk-pool-01/dataset')).toHaveValue('bulk-pool-01/advanced')

    // …but the notice names exactly what a continued Form-mode edit drops.
    const notice = screen.getByText(/the form has no controls for/i)
    expect(notice).toHaveTextContent('feasibilityOverhead')
    expect(notice).toHaveTextContent('library.ioWaitTimeoutSeconds')
  })

  it('shows no dropped-field notice when the JSON carries only form-modeled fields', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))

    const config = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/plain' } }],
      copies: 2,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [], tapeCapacityBytes: 2500000000000 },
      redundancy: { targetPercentage: 10, sliceSizeBytes: 1 },
      encryption: { recipients: ['age1pq1abc'], identity: 'AGE-SECRET-KEY-PQ-1x' },
      delivery: { webhookUrl: 'https://discord.com/api/webhooks/1/a' },
    }

    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: JSON.stringify(config) } })
    fireEvent.click(screen.getByRole('button', { name: 'Form' }))

    expect(screen.queryByText(/the form has no controls for/i)).not.toBeInTheDocument()
  })

  it('keeps the form unchanged and shows a notice when switching from malformed JSON to Form mode', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))
    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: 'not json' } })
    fireEvent.click(screen.getByRole('button', { name: 'Form' }))

    expect(screen.getByText(/could not be loaded into the form/i)).toBeInTheDocument()
    // Falls back to the default (unmodified) form state.
    expect(screen.getByPlaceholderText('bulk-pool-01/dataset')).toHaveValue('')
  })

  it('advances Form mode to Review only once the config validates, then submits and shows success', async () => {
    const fetchMock = renderPage({
      'POST /api/runs': { status: 201, body: { workflowId: 'backup', runId: 'run-new-1' } },
    })
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    await fillMinimalValidForm()

    fireEvent.click(screen.getByRole('button', { name: /review →/i }))

    await waitFor(() => {
      expect(screen.getByText('STEP 2 · REVIEW')).toBeInTheDocument()
    })
    expect(screen.getByText('Review before submitting')).toBeInTheDocument()

    const onViewRun = vi.fn()
    fireEvent.click(screen.getByRole('button', { name: /^submit run$/i }))

    await waitFor(() => {
      expect(screen.getByRole('status', { name: '' })).toBeTruthy()
    })
    await waitFor(() => {
      expect(screen.getByText('Run submitted.')).toBeInTheDocument()
    })
    expect(screen.getByText('run-new-1')).toBeInTheDocument()

    expect(fetchMock).toHaveBeenCalledWith(
      '/api/runs',
      expect.objectContaining({
        method: 'POST',
        body: expect.stringContaining('"dryRun":false'),
      }),
    )
    void onViewRun
  })

  it('blocks the Review transition and shows the schema issue when the config does not validate', async () => {
    renderPage({
      // A schema requiring a field Form mode never sets, to exercise the
      // blocking path itself regardless of whether the real committed
      // schema happens to be satisfiable by every default form value.
      '/api/config/schema': {
        status: 200,
        body: {
          ...testRunConfigSchema,
          $defs: {
            ...testRunConfigSchema.$defs,
            Config: {
              ...testRunConfigSchema.$defs!.Config,
              required: [...(testRunConfigSchema.$defs!.Config.required ?? []), 'feasibilityOverhead'],
            },
          },
        },
      },
    })
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    await fillMinimalValidForm()

    fireEvent.click(screen.getByRole('button', { name: /review →/i }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/feasibilityOverhead/)
    })

    // Never advanced to Review.
    expect(screen.getByText('STEP 1 · BUILD')).toBeInTheDocument()
    expect(screen.queryByText('Review before submitting')).not.toBeInTheDocument()
  })

  it('submits JSON mode directly (no Review step) and shows success', async () => {
    const fetchMock = renderPage({
      'POST /api/runs': { status: 201, body: { workflowId: 'backup', runId: 'run-json-1' } },
    })
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))

    const config = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/json' } }],
      copies: 1,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [], tapeCapacityBytes: 2500000000000 },
      redundancy: { targetPercentage: 10, sliceSizeBytes: 1 },
      encryption: { recipients: ['age1pq1abc'], identity: 'AGE-SECRET-KEY-PQ-1x' },
      delivery: { webhookUrl: 'https://discord.com/api/webhooks/1/a' },
    }
    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: JSON.stringify(config) } })

    fireEvent.click(screen.getByRole('button', { name: /^submit run$/i }))

    await waitFor(() => {
      expect(screen.getByText('Run submitted.')).toBeInTheDocument()
    })
    expect(screen.getByText('run-json-1')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledWith('/api/runs', expect.objectContaining({ method: 'POST' }))
  })

  it('reports a JSON parse error at submit time in JSON mode without contacting the server', async () => {
    const fetchMock = renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))
    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: 'not json' } })

    fireEvent.click(screen.getByRole('button', { name: /^submit run$/i }))

    // Two alerts render here on purpose — JSON mode's live parse indicator
    // and the submit-time error — so assert on the submit-time one
    // specifically rather than getByRole (which throws on multiple matches).
    await waitFor(() => {
      const alerts = screen.getAllByRole('alert')
      expect(alerts.some((alert) => /not valid json/i.test(alert.textContent ?? ''))).toBe(true)
    })

    expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false)
  })

  it('toggles dry-run and includes it in the submission body', async () => {
    const fetchMock = renderPage({
      'POST /api/runs': { status: 201, body: { workflowId: 'backup', runId: 'run-dry-1' } },
    })
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))
    fireEvent.change(screen.getByLabelText('Run config (JSON)'), {
      target: {
        value: JSON.stringify({
          sources: [{ zfsPath: { name: 'p' } }],
          copies: 1,
          library: { changer: 'c', drives: ['d'], blankSlots: [], tapeCapacityBytes: 2500000000000 },
          redundancy: { targetPercentage: 10, sliceSizeBytes: 1 },
          encryption: { recipients: ['r'], identity: 'i' },
          delivery: { webhookUrl: 'w' },
        }),
      },
    })

    fireEvent.click(screen.getByLabelText(/dry-run/i))
    fireEvent.click(screen.getByRole('button', { name: /^submit run$/i }))

    await waitFor(() => {
      expect(screen.getByText('Run submitted.')).toBeInTheDocument()
    })

    expect(fetchMock).toHaveBeenCalledWith(
      '/api/runs',
      expect.objectContaining({ body: expect.stringContaining('"dryRun":true') }),
    )
  })

  it('calls onViewRun with the new run ID when "View run" is clicked', async () => {
    const onViewRun = vi.fn()
    renderPage(
      { 'POST /api/runs': { status: 201, body: { workflowId: 'backup', runId: 'run-view-1' } } },
      onViewRun,
    )
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))
    fireEvent.change(screen.getByLabelText('Run config (JSON)'), {
      target: {
        value: JSON.stringify({
          sources: [{ zfsPath: { name: 'p' } }],
          copies: 1,
          library: { changer: 'c', drives: ['d'], blankSlots: [], tapeCapacityBytes: 2500000000000 },
          redundancy: { targetPercentage: 10, sliceSizeBytes: 1 },
          encryption: { recipients: ['r'], identity: 'i' },
          delivery: { webhookUrl: 'w' },
        }),
      },
    })
    fireEvent.click(screen.getByRole('button', { name: /^submit run$/i }))

    await waitFor(() => screen.getByText('Run submitted.'))
    fireEvent.click(screen.getByRole('button', { name: /view run/i }))

    expect(onViewRun).toHaveBeenCalledWith('run-view-1')
  })
})
