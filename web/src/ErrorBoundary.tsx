import { Component, type ErrorInfo, type ReactNode } from 'react'

// ErrorBoundary catches a render/lifecycle throw from its subtree and shows a
// styled fallback instead of letting the exception unwind past React's root,
// which unmounts the whole tree and leaves a blank white screen (React 19's
// documented behaviour for an uncaught render error). The web UI is an
// operator's live view of an in-progress backup — a single bad frame (a
// wrong-typed JSON-mode config reaching a page's render, a malformed API
// payload, a stray decode) must degrade to a recoverable in-app message, never
// a dead tab.
//
// App.tsx mounts one around each route's content (keyed on the route, so
// navigating to another route remounts a fresh boundary and clears a prior
// error) and main.tsx mounts one around the whole app as the last resort. There
// is no cross-request state to clean up (SPEC §4.2 — the UI is stateless), so
// recovery is just "navigate away, or reload".
interface ErrorBoundaryProps {
  children: ReactNode
  // label names the scope in the fallback copy (e.g. "this page" vs "the app"),
  // so the two mount points read naturally without two separate components.
  label?: string
}

interface ErrorBoundaryState {
  error: Error | null
}

export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { error: null }

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // Surfaced to the browser console for the operator/support to inspect;
    // there is no server-side error sink for the SPA (it is served static, and
    // /api/* is the run API, not a telemetry endpoint).
    console.error('tape-archiver UI error boundary caught a render error', error, info.componentStack)
  }

  render(): ReactNode {
    if (this.state.error === null) {
      return this.props.children
    }

    const scope = this.props.label ?? 'this page'

    return (
      <div role="alert" className="flex flex-1 flex-col items-center justify-center gap-3 bg-bg p-8 text-center">
        <div className="text-[14px] font-semibold text-text">Something went wrong rendering {scope}.</div>
        <p className="max-w-md text-[12.5px] text-text-dim">
          The view hit an unexpected error and stopped rendering. Your backup runs are unaffected — this is only the
          display. Try again, or reload the page.
        </p>
        {this.state.error.message ? (
          <pre className="max-w-md overflow-auto rounded-lg border border-border bg-surface-2 p-3 text-left font-mono text-[11px] text-text-dim">
            {this.state.error.message}
          </pre>
        ) : null}
        <button
          type="button"
          onClick={() => window.location.reload()}
          className="mt-1 rounded-lg border border-border-strong bg-surface px-3.5 py-2 text-[12.5px] font-semibold text-text hover:bg-surface-2"
        >
          Reload
        </button>
      </div>
    )
  }
}

export default ErrorBoundary
