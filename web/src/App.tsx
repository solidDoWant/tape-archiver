import SubmitRunForm from './SubmitRunForm'

// App is the shell for the tape-archiver web UI. Live monitoring, operator
// pause actions, run history, auth, and overall app-shell polish (routing,
// responsive layout, dark-mode toggle) all land in later sub-issues of the
// web UI epic (docs/web-ui-design.md §8); today this hosts the submit-run
// form (sub-issue 3) as the SPA's only view.
function App() {
  return (
    <div className="flex min-h-screen flex-col items-center gap-6 bg-white px-6 py-10 text-slate-900 dark:bg-slate-900 dark:text-slate-100">
      <h1 className="text-3xl font-semibold tracking-tight">tape-archiver</h1>
      <SubmitRunForm />
    </div>
  )
}

export default App
