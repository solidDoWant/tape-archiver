// App is the placeholder shell for the tape-archiver web UI. Submitting and
// monitoring backup runs, operator pause actions, run history, and auth all
// land in later sub-issues of the web UI epic (docs/web-ui-design.md §8);
// today this only proves the SPA serves and is dark-mode-capable.
function App() {
  return (
    <div className="flex min-h-screen flex-col items-center justify-center gap-4 bg-white px-6 text-center text-slate-900 dark:bg-slate-900 dark:text-slate-100">
      <h1 className="text-3xl font-semibold tracking-tight">tape-archiver</h1>
      <p className="max-w-md text-slate-600 dark:text-slate-400">
        The web UI is under construction. Submitting and monitoring backup
        runs will land here in upcoming changes.
      </p>
    </div>
  )
}

export default App
