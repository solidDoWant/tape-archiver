import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react'
import ConfigPage from './ConfigPage'
import { RouterProvider } from './router'
import { onSessionExpired } from './api'
import { resetConfigSchemaCache } from './configSchema'
import { testRunConfigSchema } from './testSchemaFixture'

// settle flushes state updates still pending from the page's fetch-on-mount
// inside act(), so they do not land after the test body returns as a "not wrapped
// in act(...)" warning. Awaiting one async act() tick drains the resolved-promise
// microtask chain those fetches sit on.
async function settle() {
  await act(async () => {})
}

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
    // Deploy-owned library devices + webhook (issue #304): ConfigPage fetches
    // these once via useUiConfig; buildConfig fills them into the submitted
    // config (they are no longer shown in the form), so a fully-configured
    // deployment is what lets a Form-mode submission validate without the
    // operator typing device paths. The topology (issue #305) drives the visible
    // blank-slot picker — its rendered slots are the "deploy config loaded"
    // signal fillMinimalValidForm waits on.
    '/api/config/ui': {
      status: 200,
      body: {
        temporalUiBaseUrl: '',
        temporalNamespace: '',
        library: { changer: '/dev/sch0', drives: ['/dev/nst0'], slotCount: 8, cleaningSlots: [], ioStationSlots: [] },
        delivery: { webhookConfigured: true, opticalBurnDrives: [] },
      },
    },
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
  // Wait for the deploy config to load: buildConfig fills the deploy-owned
  // changer/drives/webhook (issue #304) into the submitted config, so it only
  // validates once useUiConfig has resolved. Those values are no longer shown in
  // the form, so wait on the topology-driven blank-slot picker (issue #305)
  // rendering its slots — a positive DOM signal that the fetch resolved.
  await waitFor(() => expect(screen.getByRole('button', { name: 'Slot 1' })).toBeInTheDocument())

  fireEvent.change(screen.getByPlaceholderText('bulk-pool-01/dataset'), {
    target: { value: 'bulk-pool-01/photos' },
  })
  fireEvent.change(screen.getByPlaceholderText('age1pq1…'), {
    target: { value: 'age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4' },
  })
  fireEvent.change(screen.getByLabelText(/identity \/ private key/i), {
    target: { value: 'AGE-SECRET-KEY-PQ-1EXAMPLE' },
  })
  // Select blank slots: library.blankSlots is required and now minItems=1
  // (issue #321), so a config with no slot selected no longer validates — the
  // form must not advance to Review without one. Select two, matching the
  // default copies=2, so the blank count is a whole multiple of copies (each
  // logical tape needs one blank per copy) and the config validates.
  fireEvent.click(screen.getByRole('button', { name: 'Slot 1' }))
  fireEvent.click(screen.getByRole('button', { name: 'Slot 2' }))
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

    await settle()
  })

  it('renders the Form-mode editor by default when no run is active', async () => {
    renderPage()

    await waitFor(() => {
      expect(screen.getByRole('group', { name: /config input mode/i })).toBeInTheDocument()
    })

    expect(screen.getByText('STEP 1 · BUILD')).toBeInTheDocument()
    expect(screen.getByText('Sources')).toBeInTheDocument()

    await settle()
  })

  it('blocks a bare non-object JSON document at Review instead of a dead review step', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))
    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: '0' } })
    fireEvent.click(screen.getByRole('button', { name: 'Review →' }))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/must be a JSON object/i)
    })
    // It must NOT advance to a blank Review step with a no-op Submit.
    expect(screen.queryByText('STEP 2 · REVIEW')).not.toBeInTheDocument()

    await settle()
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

    await settle()
  })

  it('loads valid JSON text into the form when switching from JSON to Form mode', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))

    const config = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/from-json' } }],
      copies: 3,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [], tapeCapacityBytes: 2500000000000 },
      redundancy: { targetPercentage: 10 },
      encryption: { recipients: [], identity: '' },
      delivery: { webhookUrl: '' },
    }

    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: JSON.stringify(config) } })
    fireEvent.click(screen.getByRole('button', { name: 'Form' }))

    expect(screen.getByPlaceholderText('bulk-pool-01/dataset')).toHaveValue('bulk-pool-01/from-json')

    await settle()
  })

  it('does not revert form edits when the already-active Form tab is clicked again', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    // Serialize the form to JSON and come back to Form mode, so jsonText now
    // holds the pre-edit config.
    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))
    fireEvent.click(screen.getByRole('button', { name: 'Form' }))

    fireEvent.change(screen.getByPlaceholderText('bulk-pool-01/dataset'), {
      target: { value: 'bulk-pool-01/edited' },
    })

    // Clicking the already-active Form tab must be a no-op, not a re-parse of
    // the now-stale jsonText that discards the edit.
    fireEvent.click(screen.getByRole('button', { name: 'Form' }))

    expect(screen.getByPlaceholderText('bulk-pool-01/dataset')).toHaveValue('bulk-pool-01/edited')

    await settle()
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
      redundancy: { targetPercentage: 10 },
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

    await settle()
  })

  it('warns that a tape capacity matching no LTO generation is reset on the switch to Form', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))

    const config = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/custom-capacity' } }],
      copies: 2,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [], tapeCapacityBytes: 9999999999999 },
      redundancy: { targetPercentage: 10 },
      encryption: { recipients: ['age1pq1abc'], identity: 'AGE-SECRET-KEY-PQ-1x' },
      delivery: { webhookUrl: 'https://discord.com/api/webhooks/1/a' },
    }

    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: JSON.stringify(config) } })
    fireEvent.click(screen.getByRole('button', { name: 'Form' }))

    expect(screen.getByText(/matches no known LTO generation/i)).toBeInTheDocument()

    await settle()
  })

  it('shows no dropped-field notice when the JSON carries only form-modeled fields', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))

    const config = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/plain' } }],
      copies: 2,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [], tapeCapacityBytes: 2500000000000 },
      redundancy: { targetPercentage: 10 },
      encryption: { recipients: ['age1pq1abc'], identity: 'AGE-SECRET-KEY-PQ-1x' },
      delivery: { webhookUrl: 'https://discord.com/api/webhooks/1/a' },
    }

    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: JSON.stringify(config) } })
    fireEvent.click(screen.getByRole('button', { name: 'Form' }))

    expect(screen.queryByText(/the form has no controls for/i)).not.toBeInTheDocument()

    await settle()
  })

  it('warns that Form mode replaces the JSON device/webhook values with deploy config on a JSON → Form switch (issue #304)', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))

    // A JSON config that overrides the deploy-owned device/webhook values.
    const config = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/override' } }],
      copies: 2,
      library: { changer: '/dev/sch9', drives: ['/dev/nst8'], blankSlots: [], tapeCapacityBytes: 2500000000000 },
      redundancy: { targetPercentage: 10 },
      encryption: { recipients: ['age1pq1abc'], identity: 'AGE-SECRET-KEY-PQ-1x' },
      delivery: { webhookUrl: 'https://discord.com/api/webhooks/override/1' },
    }

    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: JSON.stringify(config) } })
    fireEvent.click(screen.getByRole('button', { name: 'Form' }))

    // The form loads, but the notice names exactly which deploy-owned fields
    // will be replaced by deploy config — JSON mode remains the escape hatch
    // that keeps them.
    const notice = screen.getByText(/form mode sources/i)
    expect(notice).toHaveTextContent('library.changer')
    expect(notice).toHaveTextContent('library.drives')
    expect(notice).toHaveTextContent('delivery.webhookUrl')

    await settle()
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

    await settle()
  })

  it('blocks Review with a field-named error when a critical field is left empty (issue #321)', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Slot 1' })).toBeInTheDocument())

    // Fill everything a valid run needs EXCEPT select a blank slot, so
    // library.blankSlots is [] — present but empty, the exact gap this fixes.
    fireEvent.change(screen.getByPlaceholderText('bulk-pool-01/dataset'), { target: { value: 'bulk-pool-01/photos' } })
    fireEvent.change(screen.getByPlaceholderText('age1pq1…'), {
      target: { value: 'age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4' },
    })
    fireEvent.change(screen.getByLabelText(/identity \/ private key/i), { target: { value: 'AGE-SECRET-KEY-PQ-1EXAMPLE' } })

    fireEvent.click(screen.getByRole('button', { name: /review →/i }))

    // It must NOT advance to Review; instead it names the offending field.
    await waitFor(() => {
      expect(screen.getByText(/does not validate against the run-config schema/i)).toBeInTheDocument()
    })
    expect(screen.getByText(/library\.blankSlots: must have at least one item/i)).toBeInTheDocument()
    expect(screen.queryByText('STEP 2 · REVIEW')).not.toBeInTheDocument()

    await settle()
  })

  it('blocks Review when the blank-slot count is not a multiple of copies', async () => {
    renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Slot 1' })).toBeInTheDocument())

    fireEvent.change(screen.getByPlaceholderText('bulk-pool-01/dataset'), { target: { value: 'bulk-pool-01/photos' } })
    fireEvent.change(screen.getByPlaceholderText('age1pq1…'), {
      target: { value: 'age1pq1zl8m99jvxqmkqq5jwgq8n6j9w66rlahzh5lrpttmr7pldgxqn7uqf4' },
    })
    fireEvent.change(screen.getByLabelText(/identity \/ private key/i), { target: { value: 'AGE-SECRET-KEY-PQ-1EXAMPLE' } })

    // Default copies is 2; select a single blank slot so the count (1) is not a
    // whole multiple of copies. This is schema-valid (minItems=1) but fails the
    // cross-field gate the server also enforces.
    fireEvent.click(screen.getByRole('button', { name: 'Slot 1' }))

    fireEvent.click(screen.getByRole('button', { name: /review →/i }))

    await waitFor(() => {
      expect(screen.getByText(/does not validate against the run-config schema/i)).toBeInTheDocument()
    })
    // The issues panel names the offending field; match its path-prefixed entry
    // specifically, since the form's inline picker warning carries the same
    // message text.
    expect(screen.getByText(/library\.blankSlots:.*not a multiple of 2 copies/i)).toBeInTheDocument()
    expect(screen.queryByText('STEP 2 · REVIEW')).not.toBeInTheDocument()

    await settle()
  })

  it('advances Form mode to Review only once the config validates, then submits and redirects to the run page', async () => {
    const onViewRun = vi.fn()
    const fetchMock = renderPage(
      { 'POST /api/runs': { status: 201, body: { workflowId: 'backup', runId: 'run-new-1' } } },
      onViewRun,
    )
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    await fillMinimalValidForm()

    fireEvent.click(screen.getByRole('button', { name: /review →/i }))

    await waitFor(() => {
      expect(screen.getByText('STEP 2 · REVIEW')).toBeInTheDocument()
    })
    expect(screen.getByText('Review before submitting')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /^submit run$/i }))

    // Submitting redirects straight to the new run's page — no intermediate
    // "Run submitted" confirmation to click through.
    await waitFor(() => {
      expect(onViewRun).toHaveBeenCalledWith('run-new-1')
    })
    expect(screen.queryByText('Run submitted.')).not.toBeInTheDocument()

    expect(fetchMock).toHaveBeenCalledWith(
      '/api/runs',
      expect.objectContaining({
        method: 'POST',
        body: expect.stringContaining('"dryRun":false'),
      }),
    )

    await settle()
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

    await settle()
  })

  // issue #285 (PR #297 review): the Review step's schema fetch is
  // session-gated like every other /api/* call, so an operator who sat on
  // this page past session expiry and then clicked Review must get the same
  // session-loss notification (and thus the AuthGate redirect to the login
  // page — App.test.tsx covers that leg) as any other data fetch, instead
  // of a dead in-place validation error.
  it('notifies session expiry when the Review step schema fetch 401s', async () => {
    renderPage({
      '/api/config/schema': { status: 401, body: { error: 'unauthorized' } },
    })
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    const listener = vi.fn()
    const unsubscribe = onSessionExpired(listener)

    await fillMinimalValidForm()
    fireEvent.click(screen.getByRole('button', { name: /review →/i }))

    await waitFor(() => {
      expect(listener).toHaveBeenCalledTimes(1)
    })

    unsubscribe()

    await settle()
  })

  it('routes JSON mode through the Review step before submitting, then redirects to the run page', async () => {
    const onViewRun = vi.fn()
    const fetchMock = renderPage(
      { 'POST /api/runs': { status: 201, body: { workflowId: 'backup', runId: 'run-json-1' } } },
      onViewRun,
    )
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))

    const config = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/json' } }],
      copies: 1,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [], tapeCapacityBytes: 2500000000000 },
      redundancy: { targetPercentage: 10 },
      encryption: { recipients: ['age1pq1abc'], identity: 'AGE-SECRET-KEY-PQ-1x' },
      delivery: { webhookUrl: 'https://discord.com/api/webhooks/1/a' },
    }
    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: JSON.stringify(config) } })

    // The primary JSON-mode button now reviews first — it must not submit.
    fireEvent.click(screen.getByRole('button', { name: /review/i }))
    expect(fetchMock).not.toHaveBeenCalledWith('/api/runs', expect.objectContaining({ method: 'POST' }))

    // The Review step shows the parsed config; only its Submit actually submits.
    await screen.findByText(/review before submitting/i)
    fireEvent.click(screen.getByRole('button', { name: /^submit run$/i }))

    await waitFor(() => {
      expect(onViewRun).toHaveBeenCalledWith('run-json-1')
    })
    expect(fetchMock).toHaveBeenCalledWith('/api/runs', expect.objectContaining({ method: 'POST' }))

    await settle()
  })

  it('reports a JSON parse error at the Review step without contacting the server', async () => {
    const fetchMock = renderPage()
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))
    fireEvent.change(screen.getByLabelText('Run config (JSON)'), { target: { value: 'not json' } })

    fireEvent.click(screen.getByRole('button', { name: /review/i }))

    // Two alerts render here on purpose — JSON mode's live parse indicator
    // and the review-time error — so assert on the review-time one
    // specifically rather than getByRole (which throws on multiple matches).
    await waitFor(() => {
      const alerts = screen.getAllByRole('alert')
      expect(alerts.some((alert) => /not valid json/i.test(alert.textContent ?? ''))).toBe(true)
    })

    // It stayed on the editor — the invalid config never reached Review or the server.
    expect(screen.queryByText(/review before submitting/i)).not.toBeInTheDocument()
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'POST')).toBe(false)

    await settle()
  })

  it('toggles dry-run and includes it in the submission body', async () => {
    const onViewRun = vi.fn()
    const fetchMock = renderPage(
      { 'POST /api/runs': { status: 201, body: { workflowId: 'backup', runId: 'run-dry-1' } } },
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
          redundancy: { targetPercentage: 10 },
          encryption: { recipients: ['r'], identity: 'i' },
          delivery: { webhookUrl: 'w' },
        }),
      },
    })

    fireEvent.click(screen.getByLabelText(/dry-run/i))
    fireEvent.click(screen.getByRole('button', { name: /review/i }))
    fireEvent.click(await screen.findByRole('button', { name: /^submit run$/i }))

    await waitFor(() => {
      expect(onViewRun).toHaveBeenCalledWith('run-dry-1')
    })

    expect(fetchMock).toHaveBeenCalledWith(
      '/api/runs',
      expect.objectContaining({ body: expect.stringContaining('"dryRun":true') }),
    )

    await settle()
  })

  it('falls back to an inline confirmation with the run ID when rendered without a navigation callback', async () => {
    // No onViewRun passed: nothing to navigate with, so the page keeps the run
    // ID visible inline instead of redirecting.
    renderPage({ 'POST /api/runs': { status: 201, body: { workflowId: 'backup', runId: 'run-view-1' } } })
    await waitFor(() => screen.getByRole('group', { name: /config input mode/i }))

    fireEvent.click(screen.getByRole('button', { name: 'Paste / upload' }))
    fireEvent.change(screen.getByLabelText('Run config (JSON)'), {
      target: {
        value: JSON.stringify({
          sources: [{ zfsPath: { name: 'p' } }],
          copies: 1,
          library: { changer: 'c', drives: ['d'], blankSlots: [], tapeCapacityBytes: 2500000000000 },
          redundancy: { targetPercentage: 10 },
          encryption: { recipients: ['r'], identity: 'i' },
          delivery: { webhookUrl: 'w' },
        }),
      },
    })
    fireEvent.click(screen.getByRole('button', { name: /review/i }))
    fireEvent.click(await screen.findByRole('button', { name: /^submit run$/i }))

    await waitFor(() => expect(screen.getByText('Run submitted.')).toBeInTheDocument())
    expect(screen.getByText('run-view-1')).toBeInTheDocument()
    // Without a navigation callback there is no "View run" affordance.
    expect(screen.queryByRole('button', { name: /view run/i })).not.toBeInTheDocument()

    await settle()
  })

  it('preloads a prior run’s config for a restart, blanking the redacted age identity', async () => {
    const priorConfig = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/archive@snap' }, label: 'archive', compression: true }],
      copies: 2,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [1, 2], tapeCapacityBytes: 2500000000000 },
      redundancy: { targetPercentage: 10 },
      // The server redacts the age identity (a private key) — the restart must
      // never load the placeholder into the form as if it were a real key.
      encryption: { recipients: ['age1restarttestrecipient00000000000000000000000000000000000'], identity: '***redacted***' },
      delivery: { webhookUrl: '***redacted***' },
    }

    stubApi({ '/api/runs/old-run/config': { status: 200, body: { runId: 'old-run', config: priorConfig, dryRun: false } } })

    render(
      <RouterProvider>
        <ConfigPage restartFromRunId="old-run" />
      </RouterProvider>,
    )

    // The prior config's own values land in the form...
    expect(await screen.findByDisplayValue('bulk-pool-01/archive@snap')).toBeInTheDocument()
    expect(screen.getByDisplayValue('age1restarttestrecipient00000000000000000000000000000000000')).toBeInTheDocument()

    // ...with a notice explaining the restart and the redacted identity...
    expect(screen.getByText(/Loaded the configuration from run old-run/i)).toBeInTheDocument()
    expect(screen.getByText(/re-enter it before submitting/i)).toBeInTheDocument()

    // ...and the redacted placeholder is never loaded into any field.
    expect(screen.queryByDisplayValue('***redacted***')).not.toBeInTheDocument()

    // A restart of a production run leaves Dry-run off (its default).
    expect(screen.getByLabelText(/dry-run/i)).not.toBeChecked()

    await settle()
  })

  it('carries the dry-run flag over when restarting a dry-run', async () => {
    const priorConfig = {
      sources: [{ zfsPath: { name: 'bulk-pool-01/archive@snap' }, compression: true }],
      copies: 1,
      library: { changer: '/dev/sch0', drives: ['/dev/nst0'], blankSlots: [1], tapeCapacityBytes: 2500000000000 },
      redundancy: { targetPercentage: 10 },
      encryption: { recipients: ['age1restarttestrecipient00000000000000000000000000000000000'], identity: '***redacted***' },
      delivery: {},
    }

    stubApi({ '/api/runs/dry-old/config': { status: 200, body: { runId: 'dry-old', config: priorConfig, dryRun: true } } })

    render(
      <RouterProvider>
        <ConfigPage restartFromRunId="dry-old" />
      </RouterProvider>,
    )

    // The config loads, and because the source run was a dry-run the toggle is
    // re-selected (rather than defaulting a re-run of a dry-run to production)...
    expect(await screen.findByDisplayValue('bulk-pool-01/archive@snap')).toBeInTheDocument()
    expect(screen.getByLabelText(/dry-run/i)).toBeChecked()

    // ...and the notice says so.
    expect(screen.getByText(/this run was a dry-run/i)).toBeInTheDocument()

    await settle()
  })

  it('surfaces an error but still lets the operator build a run when the restart config can’t be loaded', async () => {
    stubApi({ '/api/runs/gone/config': { status: 410, body: { error: 'run history has aged out' } } })

    render(
      <RouterProvider>
        <ConfigPage restartFromRunId="gone" />
      </RouterProvider>,
    )

    expect(await screen.findByRole('alert')).toHaveTextContent(/could not load run gone.s configuration/i)
    // The form is still usable — the restart failure is non-fatal.
    expect(screen.getByRole('button', { name: /review/i })).toBeInTheDocument()

    await settle()
  })
})
