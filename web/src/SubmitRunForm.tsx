import { useState, type ChangeEvent, type FormEvent } from 'react'

export interface SubmitRunResult {
  workflowId: string
  runId: string
}

interface SubmitState {
  status: 'idle' | 'submitting' | 'success' | 'error'
  result?: SubmitRunResult
  error?: string
}

export interface SubmitRunFormProps {
  // onViewRun, if given, renders a "View run" link on a successful
  // submission's result that calls back with the new run's ID — the "linked
  // run detail view" this component's doc comment used to defer to a later
  // sub-issue (now RunDetail.tsx, sub-issue 4). Optional so this component
  // stays usable standalone (e.g. in tests) without a navigation callback.
  onViewRun?: (runId: string) => void
}

// SubmitRunForm lets an operator paste or upload a run-config JSON document,
// optionally mark it as a dry-run (redirected to the mhvtl virtual library,
// optical burning disabled), and submit it to POST /api/runs (pkg/runsapi) —
// the same validation, dry-run override, and singleton-conflict handling
// `tapectl run [--dry-run]` uses (pkg/runsubmit), so this form and the CLI
// can never diverge on what a submission means
// (docs/web-ui-design.md §2, §3, §8 item 3). Resume/abort actions land in a
// later sub-issue of the web UI epic; today a successful submission shows
// the returned run ID and, when onViewRun is given, a link straight to its
// live RunDetail view (sub-issue 4), and a failure shows the API's error
// message verbatim.
function SubmitRunForm({ onViewRun }: SubmitRunFormProps) {
  const [configText, setConfigText] = useState('')
  const [dryRun, setDryRun] = useState(false)
  const [state, setState] = useState<SubmitState>({ status: 'idle' })

  const handleFileChange = (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]

    // Allow re-selecting the same file again later regardless of outcome.
    event.target.value = ''

    if (!file) {
      return
    }

    file
      .text()
      .then((text) => setConfigText(text))
      .catch(() => {
        setState({ status: 'error', error: 'Could not read the selected file.' })
      })
  }

  const handleSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()

    let config: unknown

    try {
      config = JSON.parse(configText)
    } catch (parseError) {
      const message = parseError instanceof Error ? parseError.message : String(parseError)
      setState({ status: 'error', error: `The run config is not valid JSON: ${message}` })

      return
    }

    setState({ status: 'submitting' })

    try {
      const response = await fetch('/api/runs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ config, dryRun }),
      })

      const body = await response.json().catch(() => null)

      if (!response.ok) {
        const message =
          body && typeof body.error === 'string'
            ? body.error
            : `Submission failed with status ${response.status}.`
        setState({ status: 'error', error: message })

        return
      }

      if (
        !body ||
        typeof body.workflowId !== 'string' ||
        typeof body.runId !== 'string'
      ) {
        // A 2xx with an empty/malformed body (e.g. a proxy stripping it) is
        // not a network failure — the run may well have been submitted — but
        // it can't be reported as a success either, since there's no run ID
        // to show. Report it plainly instead of letting a property access
        // throw here and get misreported as "could not reach the server" by
        // the catch below.
        setState({
          status: 'error',
          error: 'The server accepted the submission but returned an unreadable response.',
        })

        return
      }

      setState({ status: 'success', result: { workflowId: body.workflowId, runId: body.runId } })
    } catch (networkError) {
      const message = networkError instanceof Error ? networkError.message : String(networkError)
      setState({ status: 'error', error: `Could not reach the server: ${message}` })
    }
  }

  const submitting = state.status === 'submitting'
  const result = state.status === 'success' ? state.result : undefined

  return (
    <form
      onSubmit={(event) => {
        void handleSubmit(event)
      }}
      aria-label="Submit backup run"
      className="flex w-full max-w-2xl flex-col gap-4 text-left"
    >
      <div className="flex flex-col gap-2">
        <label htmlFor="run-config" className="font-medium">
          Run config (JSON)
        </label>
        <textarea
          id="run-config"
          value={configText}
          onChange={(event) => setConfigText(event.target.value)}
          rows={12}
          spellCheck={false}
          placeholder="Paste run-config JSON here, or upload a file below."
          className="w-full rounded border border-slate-300 bg-white p-2 font-mono text-sm text-slate-900 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-100"
        />
        <input
          type="file"
          accept="application/json,.json"
          onChange={handleFileChange}
          aria-label="Upload run config file"
        />
      </div>

      <label className="flex items-center gap-2">
        <input
          type="checkbox"
          checked={dryRun}
          onChange={(event) => setDryRun(event.target.checked)}
        />
        Dry-run (redirect to the mhvtl virtual library; disables optical burning)
      </label>

      <button
        type="submit"
        disabled={submitting || configText.trim() === ''}
        className="self-start rounded bg-slate-900 px-4 py-2 font-medium text-white disabled:opacity-50 dark:bg-slate-100 dark:text-slate-900"
      >
        {submitting ? 'Submitting…' : 'Submit run'}
      </button>

      {result ? (
        <div
          role="status"
          className="rounded border border-green-600 bg-green-50 p-3 text-green-900 dark:border-green-500 dark:bg-green-950 dark:text-green-100"
        >
          <p className="font-medium">Run submitted.</p>
          <p>
            Run ID: <code>{result.runId}</code>
          </p>
          <p>
            Workflow ID: <code>{result.workflowId}</code>
          </p>
          {onViewRun ? (
            <button
              type="button"
              onClick={() => onViewRun(result.runId)}
              className="mt-2 font-medium underline"
            >
              View run
            </button>
          ) : null}
        </div>
      ) : null}

      {state.status === 'error' && state.error ? (
        <div
          role="alert"
          className="rounded border border-red-600 bg-red-50 p-3 text-red-900 dark:border-red-500 dark:bg-red-950 dark:text-red-100"
        >
          {state.error}
        </div>
      ) : null}
    </form>
  )
}

export default SubmitRunForm
