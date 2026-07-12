import { useState } from 'react'
import { apiFetch, ApiError, describeNetworkError } from './api'
import { useActiveRun } from './activeRun'
import {
  blankSlotsCopiesIssue,
  buildConfig,
  configToFormState,
  defaultFormState,
  deployOwnedFields,
  unmodeledFields,
  type FormState,
  type RunConfig,
} from './configModel'
import { deployConfigFrom, useUiConfig } from './uiConfig'
import { fetchConfigSchema, validateAgainstSchema, type ValidationIssue } from './configSchema'
import ConfigForm from './ConfigForm'
import ConfigReview from './ConfigReview'
import ConfigJsonMode from './ConfigJsonMode'
import { Link } from './router'
import { runPath } from './route'

export interface ConfigPageProps {
  onViewRun?: (runId: string) => void
}

type Mode = 'form' | 'json'
type Step = 'edit' | 'review'

interface SubmitResult {
  workflowId: string
  runId: string
}

type SubmitState =
  | { status: 'idle' }
  | { status: 'submitting' }
  | { status: 'success'; result: SubmitResult }
  | { status: 'error'; error: string }

const segmentedWrapClass = 'flex w-fit items-center gap-0.5 rounded-lg border border-border bg-inset p-0.5'

function segmentButtonClass(active: boolean): string {
  return active
    ? 'rounded-md border border-border-strong bg-surface px-3.5 py-1.5 font-mono text-[11px] font-semibold text-text'
    : 'rounded-md border border-transparent px-3.5 py-1.5 font-mono text-[11px] font-semibold text-text-faint transition-colors hover:text-text'
}

// ConfigPage is the "Start new run" page (route "/", DESIGN_ANALYSIS.md §2
// "D. Config / Start new run"), replacing the former JSON-only
// SubmitRunForm.tsx (issue #279). It offers two ways to build a run config —
// a guided Form mode (ConfigForm.tsx) that ends in a Review step
// (ConfigReview.tsx) showing the assembled JSON validated against the
// committed schema before it can be submitted, and a JSON mode
// (ConfigJsonMode.tsx) preserving today's paste/upload + live valid
// indicator — sharing one sticky action bar (dry-run toggle, Review/Submit
// controls) and one submission path (POST /api/runs, unchanged from before
// this issue).
//
// Mode-switch semantics (documented here since nothing enforces them beyond
// this code — see docs/web-ui-design.md §9 for the same decision recorded
// for operators/reviewers): switching Form → JSON always re-serializes the
// current form state into the JSON textarea (so it reflects the latest
// edits, not stale prior JSON text); switching JSON → Form attempts to
// parse the current JSON text and populate the form from it
// (configModel.ts's configToFormState) when it parses as an object, and
// otherwise leaves the form state exactly as it was before the switch (a
// syntactically broken JSON document has nothing coherent to map into
// fields) — either way nothing is silently discarded: the operator can
// always switch back to see what they last had in the other mode's own
// state, since neither mode's state is cleared by switching away from it.
// The one asymmetry is called out loudly rather than papered over: the form
// has no controls for a few advanced fields (configModel.ts's
// unmodeledFields — feasibilityOverhead and the operator-wait timeout
// overrides), so a JSON → Form switch of a config carrying one shows a
// notice naming exactly which fields a continued Form-mode edit would drop.
//
// AC5 (a run already in progress blocks the whole page, not just submit) is
// answered here by the same useActiveRun one-shot check the sidebar already
// uses (issue #272) — no new endpoint or live subscription.
function ConfigPage({ onViewRun }: ConfigPageProps) {
  const activeRunState = useActiveRun()
  // Deploy-owned library devices + Discord webhook (issue #304): Form mode
  // shows them read-only and buildConfig fills them into the submitted config,
  // rather than the operator re-typing them per run. One cached fetch, same
  // pattern as the run overview's Temporal-UI link.
  const uiConfigState = useUiConfig()
  const deploy = deployConfigFrom(uiConfigState)

  const [mode, setMode] = useState<Mode>('form')
  const [step, setStep] = useState<Step>('edit')
  const [form, setForm] = useState<FormState>(defaultFormState)
  const [jsonText, setJsonText] = useState('')
  const [dryRun, setDryRun] = useState(false)
  const [reviewIssues, setReviewIssues] = useState<ValidationIssue[]>([])
  const [validating, setValidating] = useState(false)
  const [modeSwitchNotice, setModeSwitchNotice] = useState('')
  const [submitState, setSubmitState] = useState<SubmitState>({ status: 'idle' })

  const switchToJson = () => {
    setJsonText(JSON.stringify(buildConfig(form, deploy), null, 2))
    setModeSwitchNotice('')
    setMode('json')
    setStep('edit')
  }

  const switchToForm = () => {
    try {
      const parsed = JSON.parse(jsonText) as unknown

      if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        throw new Error('not a config object')
      }

      const config = parsed as RunConfig

      setForm(configToFormState(config))

      // Two kinds of field don't survive a JSON → Form switch, called out by
      // name up front rather than changed silently: (1) advanced fields the
      // form has no controls for (unmodeledFields) survive only as long as the
      // JSON text itself and are dropped once Form mode re-serializes; (2) the
      // deploy-owned device/webhook fields (deployOwnedFields, issue #304) are
      // replaced by the deployment's own config, since Form mode sources them
      // from deploy config rather than the JSON — and, where this deployment
      // configures them, the server applies its own values to every submitted
      // run regardless of mode (pkg/runsapi applyDeployConfig), so JSON / paste
      // mode is no longer an override for them either.
      const dropped = unmodeledFields(config)
      const deployOwned = deployOwnedFields(config)

      const notices: string[] = []

      if (dropped.length > 0) {
        notices.push(
          `The form has no controls for ${dropped.join(', ')} — continuing in Form mode drops ${dropped.length === 1 ? 'this field' : 'these fields'} (switch back to JSON mode to keep ${dropped.length === 1 ? 'it' : 'them'}).`,
        )
      }

      if (deployOwned.length > 0) {
        notices.push(
          `Form mode sources ${deployOwned.join(', ')} from this deployment's config, replacing the ${deployOwned.length === 1 ? 'value' : 'values'} in this JSON. Where this deployment configures ${deployOwned.length === 1 ? 'it' : 'them'}, the server applies its own ${deployOwned.length === 1 ? 'value' : 'values'} to every run — JSON / paste mode included.`,
        )
      }

      setModeSwitchNotice(notices.join(' '))
    } catch {
      setModeSwitchNotice(
        'The current JSON could not be loaded into the form (it is not valid JSON), so the form keeps its last state.',
      )
    }

    setMode('form')
  }

  const handleReview = async () => {
    setValidating(true)

    try {
      const schema = await fetchConfigSchema()
      const config = buildConfig(form, deploy)
      const issues = validateAgainstSchema(schema, config)

      // The blank-slots-multiple-of-copies rule is a cross-field invariant the
      // schema validator deliberately does not encode (see configSchema.ts), so
      // gate it here too — matching the server's internal/config/validate.go —
      // rather than only warning inline in the form, so it can never be clicked
      // past into Review.
      const copiesIssue = blankSlotsCopiesIssue(config.copies, config.library.blankSlots.length)
      if (copiesIssue !== null) {
        issues.push({ path: 'library.blankSlots', message: copiesIssue })
      }

      setReviewIssues(issues)

      if (issues.length === 0) {
        setStep('review')
      }
    } catch (error) {
      setReviewIssues([
        { path: '', message: `Could not validate against the run-config schema: ${describeNetworkError(error)}` },
      ])
    } finally {
      setValidating(false)
    }
  }

  const submit = async (config: unknown) => {
    setSubmitState({ status: 'submitting' })

    try {
      const result = await apiFetch<{ workflowId?: string; runId?: string }>('/api/runs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ config, dryRun }),
      })

      if (!result.workflowId || !result.runId) {
        setSubmitState({
          status: 'error',
          error: 'The server accepted the submission but returned an unreadable response.',
        })

        return
      }

      // Go straight to the new run's page rather than parking on an intermediate
      // confirmation the operator would have to click through — a submitted run
      // is a singleton, so the run page is the only place they'd head next. When
      // the page is mounted without a navigation callback (standalone/tests),
      // fall back to an inline confirmation so the run ID is not lost.
      if (onViewRun) {
        onViewRun(result.runId)

        return
      }

      setSubmitState({ status: 'success', result: { workflowId: result.workflowId, runId: result.runId } })
    } catch (error) {
      const message = error instanceof ApiError ? error.message : describeNetworkError(error)
      setSubmitState({ status: 'error', error: message })
    }
  }

  const handleSubmitReview = () => {
    void submit(buildConfig(form, deploy))
  }

  const handleSubmitJson = () => {
    let config: unknown

    try {
      config = JSON.parse(jsonText)
    } catch (parseError) {
      const message = parseError instanceof Error ? parseError.message : String(parseError)
      setSubmitState({ status: 'error', error: `The run config is not valid JSON: ${message}` })

      return
    }

    void submit(config)
  }

  if (activeRunState.status === 'loading') {
    return (
      <div className="p-6 sm:p-7">
        <p role="status" className="text-[12.5px] text-text-dim">
          Checking for an active run…
        </p>
      </div>
    )
  }

  if (activeRunState.status === 'loaded' && activeRunState.activeRun) {
    const activeRun = activeRunState.activeRun

    return (
      <div className="max-w-2xl p-6 sm:p-7">
        <div className="flex flex-col items-center rounded-xl border border-border bg-surface p-11 text-center shadow-card">
          <div className="mb-4 flex h-13 w-13 items-center justify-center rounded-2xl border border-amber-line bg-amber-bg text-[22px]">
            ⏸
          </div>
          <div className="text-[17px] font-semibold text-text">A run is already in progress</div>
          <p className="mt-2.5 max-w-md text-[13px] text-text-dim">
            Backup runs are a singleton — the tool runs one at a time and refuses any new submission until the
            current run finishes. Wait for it to complete, or resume/abort it from the current run.
          </p>
          <Link
            to={runPath(activeRun.runId)}
            className="mt-5 rounded-lg bg-text px-4.5 py-2.25 text-[12.5px] font-semibold text-bg"
          >
            Open current run
          </Link>
        </div>
      </div>
    )
  }

  const submitting = submitState.status === 'submitting'
  const success = submitState.status === 'success' ? submitState.result : undefined

  return (
    <div className="flex w-full max-w-3xl flex-col gap-4 p-6 sm:p-7">
      <div className="flex flex-wrap items-center gap-3">
        <span className="rounded-full border border-border-strong bg-inset px-2.5 py-1 font-mono text-[11px] font-semibold text-text-dim">
          {step === 'review' ? 'STEP 2 · REVIEW' : 'STEP 1 · BUILD'}
        </span>
        <span className="flex-1" />
        {step === 'edit' ? (
          <div className={segmentedWrapClass} role="group" aria-label="Config input mode">
            <button type="button" className={segmentButtonClass(mode === 'form')} onClick={switchToForm}>
              Form
            </button>
            <button type="button" className={segmentButtonClass(mode === 'json')} onClick={switchToJson}>
              Paste / upload
            </button>
          </div>
        ) : null}
      </div>

      {modeSwitchNotice ? (
        <p role="status" className="text-[11.5px] text-amber">
          {modeSwitchNotice}
        </p>
      ) : null}

      {step === 'edit' && mode === 'form' ? <ConfigForm form={form} setForm={setForm} deploy={deploy} deployStatus={uiConfigState.status} /> : null}
      {step === 'edit' && mode === 'json' ? <ConfigJsonMode text={jsonText} onTextChange={setJsonText} /> : null}
      {step === 'review' ? <ConfigReview config={buildConfig(form, deploy)} dryRun={dryRun} /> : null}

      {reviewIssues.length > 0 ? (
        <div role="alert" className="rounded-lg border border-red-line bg-red-bg p-3.5 text-[12px] text-red">
          <p className="font-semibold">This config does not validate against the run-config schema:</p>
          <ul className="mt-1.5 list-inside list-disc font-mono text-[11.5px]">
            {reviewIssues.map((issue) => (
              <li key={`${issue.path}:${issue.message}`}>
                {issue.path || '(root)'}: {issue.message}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <div className="sticky bottom-0 rounded-xl border border-border bg-bg/90 p-3.5 shadow-card backdrop-blur-sm">
        <div className="flex flex-wrap items-center gap-3.5">
          <label className="flex items-center gap-2 text-[12.5px] text-text">
            <input
              type="checkbox"
              checked={dryRun}
              onChange={(event) => setDryRun(event.target.checked)}
              className="h-4 w-4 accent-blue"
            />
            Dry-run <span className="font-mono text-[11px] text-text-faint">· mhvtl</span>
          </label>

          <span className="flex-1" />

          {step === 'edit' && mode === 'form' ? (
            <button
              type="button"
              onClick={() => void handleReview()}
              disabled={validating}
              className="rounded-lg bg-text px-5 py-2.25 text-[12.5px] font-semibold text-bg disabled:opacity-50"
            >
              {validating ? 'Validating…' : 'Review →'}
            </button>
          ) : null}

          {step === 'edit' && mode === 'json' ? (
            <button
              type="button"
              onClick={handleSubmitJson}
              disabled={submitting || jsonText.trim() === ''}
              className="rounded-lg bg-text px-5 py-2.25 text-[12.5px] font-semibold text-bg disabled:opacity-50"
            >
              {submitting ? 'Submitting…' : 'Submit run'}
            </button>
          ) : null}

          {step === 'review' ? (
            <>
              <button
                type="button"
                onClick={() => setStep('edit')}
                className="rounded-lg border border-border-strong bg-surface px-4.5 py-2.25 text-[12.5px] font-medium text-text transition-colors hover:bg-surface-2"
              >
                ← Back to edit
              </button>
              <button
                type="button"
                onClick={handleSubmitReview}
                disabled={submitting}
                className="rounded-lg bg-text px-5 py-2.25 text-[12.5px] font-semibold text-bg disabled:opacity-50"
              >
                {submitting ? 'Submitting…' : 'Submit run'}
              </button>
            </>
          ) : null}
        </div>
      </div>

      {success ? (
        // Only reached when the page has no navigation callback (a successful
        // submission otherwise redirects straight to the run page); this is the
        // standalone fallback that keeps the run ID visible.
        <div
          role="status"
          className="rounded-lg border border-green-line bg-green-bg p-3.5 text-[12.5px] text-green"
        >
          <p className="font-semibold">Run submitted.</p>
          <p className="mt-1">
            Run ID: <code>{success.runId}</code>
          </p>
          <p>
            Workflow ID: <code>{success.workflowId}</code>
          </p>
        </div>
      ) : null}

      {submitState.status === 'error' ? (
        <div role="alert" className="rounded-lg border border-red-line bg-red-bg p-3.5 text-[12.5px] text-red">
          {submitState.error}
        </div>
      ) : null}
    </div>
  )
}

export default ConfigPage
