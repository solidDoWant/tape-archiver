import { useState } from 'react'
import { apiFetch, ApiError, describeNetworkError } from './api'

export interface AgeKeypair {
  recipient: string
  identity: string
}

export interface AgeKeygenPanelProps {
  // onGenerated fires once, right after a successful POST /api/age/keygen,
  // so the caller (ConfigForm's encryption section) can insert the new
  // recipient into the config's recipients list and the identity into its
  // identity field — "the public key is placed into the config" (issue
  // #279's acceptance criterion). This component itself never writes into
  // those fields directly; it only ever shows the raw keypair it was just
  // handed, once.
  onGenerated: (keypair: AgeKeypair) => void
}

type KeygenState =
  | { status: 'idle' }
  | { status: 'generating' }
  | { status: 'generated'; keypair: AgeKeypair }
  | { status: 'error'; error: string }

// AgeKeygenPanel is the config page's "Generate new age keypair" control
// (DESIGN_ANALYSIS.md §2 "D. Config", encryption section): a button that
// calls POST /api/age/keygen (pkg/runsapi/agekeygen.go) and, on success,
// reveals the generated identity/recipient pair exactly once, with a copy
// control — matching issue #279's acceptance criterion that "the private key
// is displayed once with no way to retrieve it again from the app afterward".
// This component holds the only copy of it that ever exists client-side: it is
// never written to localStorage/sessionStorage, never logged, and the server
// never persists it (agekeygen.go's own doc comment) or exposes any endpoint to
// fetch it again — reloading the page, navigating away, or generating a second
// keypair all permanently lose access to a previously shown one.
//
// It carries no "store this now or lose it forever" warning: the identity is
// deliberately escrowed into every completed run's report and recovery ISO (and
// so into the report delivered to Discord — SPEC §7), so it is not irrecoverable
// once a run has run.
function AgeKeygenPanel({ onGenerated }: AgeKeygenPanelProps) {
  const [state, setState] = useState<KeygenState>({ status: 'idle' })
  const [copied, setCopied] = useState(false)

  const generate = async () => {
    setState({ status: 'generating' })
    setCopied(false)

    try {
      const keypair = await apiFetch<AgeKeypair>('/api/age/keygen', { method: 'POST' })
      setState({ status: 'generated', keypair })
      onGenerated(keypair)
    } catch (error) {
      const message = error instanceof ApiError ? error.message : describeNetworkError(error)
      setState({ status: 'error', error: message })
    }
  }

  const copyIdentity = async (identity: string) => {
    try {
      await navigator.clipboard.writeText(identity)
      setCopied(true)
    } catch {
      // Clipboard access can be denied/unavailable (permissions, insecure
      // context, jsdom in tests); the identity stays selectable/copyable by
      // hand in the panel below regardless, so this is a silent no-op
      // rather than a second error state layered over the keygen result.
    }
  }

  const generating = state.status === 'generating'

  return (
    <div className="flex flex-col gap-2">
      <button
        type="button"
        onClick={() => void generate()}
        disabled={generating}
        className="self-start rounded-lg border border-border-strong bg-surface-2 px-3.5 py-1.5 text-[11.5px] font-medium text-text transition-colors hover:border-text-faint hover:bg-inset disabled:opacity-50"
      >
        {generating ? 'Generating…' : 'Generate new age keypair'}
      </button>

      {state.status === 'error' ? (
        <div
          role="alert"
          className="rounded-lg border border-red-line bg-red-bg p-3 text-[11.5px] text-red"
        >
          {state.error}
        </div>
      ) : null}

      {state.status === 'generated' ? (
        <div role="status" className="rounded-lg border border-blue bg-blue-bg p-3">
          <span className="font-mono text-[11px] font-semibold text-blue">NEW KEYPAIR GENERATED</span>

          <dl className="mt-2 flex flex-col gap-2 font-mono text-[11px] text-text">
            <div>
              <dt className="text-text-faint">recipient (public)</dt>
              <dd className="break-all">{state.keypair.recipient}</dd>
            </div>
            <div>
              <dt className="text-text-faint">identity / private key</dt>
              <dd className="break-all">{state.keypair.identity}</dd>
            </div>
          </dl>

          <div className="mt-2.5 flex gap-2">
            <button
              type="button"
              onClick={() => void copyIdentity(state.keypair.identity)}
              className="rounded-md border border-border-strong bg-surface px-3 py-1.5 text-[11px] font-medium text-text transition-colors hover:bg-surface-2"
            >
              {copied ? 'Copied' : 'Copy identity'}
            </button>
          </div>
        </div>
      ) : null}
    </div>
  )
}

export default AgeKeygenPanel
