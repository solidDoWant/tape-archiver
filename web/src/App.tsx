import { useState } from 'react'
import SubmitRunForm from './SubmitRunForm'
import RunDetail from './RunDetail'

// runIDFromLocation reads a run ID straight out of window.location.pathname
// (matching "/runs/{runID}"), so a direct link or a page refresh while
// viewing a run's detail lands back on that same view rather than always
// resetting to the submit form. This is deliberately not a general router:
// operator pause actions, run history, and overall app-shell polish
// (routing, responsive layout, dark-mode toggle) land in later sub-issues of
// the web UI epic (docs/web-ui-design.md §8) — sub-issue 7 in particular
// owns real client-side routing and will very likely replace this. Until
// then, App holds "which run (if any) is being viewed" as state lifted here
// and navigates by calling history.pushState directly (see
// navigateToRun/navigateToSubmit below) — the smallest mechanism that lets a
// submitted run's detail actually be viewed, without building out a general
// route matcher this issue does not need. Browser back/forward (popstate) is
// intentionally not wired up; that is left to sub-issue 7's real router.
function runIDFromLocation(): string | null {
  const match = /^\/runs\/([^/]+)\/?$/.exec(window.location.pathname)

  return match ? decodeURIComponent(match[1]) : null
}

// App is the shell for the tape-archiver web UI.
function App() {
  const [runID, setRunID] = useState<string | null>(() => runIDFromLocation())

  const navigateToRun = (id: string) => {
    window.history.pushState({}, '', `/runs/${encodeURIComponent(id)}`)
    setRunID(id)
  }

  const navigateToSubmit = () => {
    window.history.pushState({}, '', '/')
    setRunID(null)
  }

  return (
    <div className="flex min-h-screen flex-col items-center gap-6 bg-white px-6 py-10 text-slate-900 dark:bg-slate-900 dark:text-slate-100">
      <h1 className="text-3xl font-semibold tracking-tight">tape-archiver</h1>
      {runID ? (
        // key={runID}: forces a fresh RunDetail mount (and thus a fresh
        // EventSource + reset display state) whenever the viewed run
        // changes, rather than RunDetail resetting its own state from
        // inside an effect on a prop change — see RunDetail's doc comment.
        <RunDetail key={runID} runId={runID} onBack={navigateToSubmit} />
      ) : (
        <SubmitRunForm onViewRun={navigateToRun} />
      )}
    </div>
  )
}

export default App
