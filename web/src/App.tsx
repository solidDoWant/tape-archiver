import SubmitRunForm from './SubmitRunForm'
import RunDetail from './RunDetail'
import RunHistory from './RunHistory'
import { Link, RouterProvider } from './router'
import { runPath, useNavigate, useRoute, type Route } from './route'
import { useTheme } from './theme'

// App is the persistent shell for the tape-archiver web UI: a header with
// app-wide navigation (reachable from every view, satisfying this issue's
// "app shell" acceptance criterion) wrapping a router-driven content area.
//
// Earlier sub-issues (4, 5) built RunDetail/PauseActions against an ad hoc
// navigation mechanism this file previously documented in detail: a single
// lifted `runID` bit of state and direct `history.pushState` calls, with no
// `popstate` handling and no history-list view — explicitly deferring "real
// client-side routing" and "run history" to this sub-issue. This is that
// replacement; see router.tsx for the routing approach (a small hand-rolled
// router rather than a dependency) and its rationale.
function App() {
  return (
    <RouterProvider>
      <Shell />
    </RouterProvider>
  )
}

// navLinkClass highlights whichever nav link matches the current route, so
// the shell doubles as a lightweight "you are here" indicator.
function navLinkClass(active: boolean): string {
  const base = 'rounded px-3 py-1.5 text-sm font-medium'

  return active
    ? `${base} bg-slate-900 text-white dark:bg-slate-100 dark:text-slate-900`
    : `${base} text-slate-700 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800`
}

// Shell renders the header/nav and the current route's view. The header is
// flex-wrap so it stays usable at mobile widths without ever needing
// horizontal scroll — a narrow viewport wraps the nav links onto their own
// line below the title rather than clipping or overflowing.
function Shell() {
  const route = useRoute()
  const navigate = useNavigate()
  const [theme, setTheme] = useTheme()

  return (
    <div className="flex min-h-screen flex-col bg-white text-slate-900 dark:bg-slate-900 dark:text-slate-100">
      <header className="flex flex-wrap items-center justify-between gap-3 border-b border-slate-200 px-4 py-3 sm:px-6 dark:border-slate-800">
        <Link to="/" className="text-xl font-semibold tracking-tight">
          tape-archiver
        </Link>

        <nav aria-label="Main" className="flex flex-wrap items-center gap-2">
          <Link to="/" className={navLinkClass(route.name === 'submit')}>
            Submit
          </Link>
          <Link to="/history" className={navLinkClass(route.name === 'history')}>
            History
          </Link>
          <button
            type="button"
            onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}
            aria-label={theme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'}
            className="rounded border border-slate-300 px-3 py-1.5 text-sm font-medium text-slate-700 hover:bg-slate-100 dark:border-slate-700 dark:text-slate-300 dark:hover:bg-slate-800"
          >
            {theme === 'dark' ? '☀️ Light' : '🌙 Dark'}
          </button>
        </nav>
      </header>

      <main className="flex flex-1 flex-col items-center gap-6 px-4 py-8 sm:px-6">
        {renderRoute(route, navigate)}
      </main>
    </div>
  )
}

function renderRoute(route: Route, navigate: (path: string) => void) {
  switch (route.name) {
    case 'submit':
      return <SubmitRunForm onViewRun={(runId) => navigate(runPath(runId))} />
    case 'history':
      return <RunHistory />
    case 'run':
      // key={route.runId}: forces a fresh RunDetail mount (and thus a fresh
      // EventSource + reset display state) whenever the viewed run changes,
      // rather than RunDetail resetting its own state from inside an effect
      // on a prop change — see RunDetail's doc comment.
      return <RunDetail key={route.runId} runId={route.runId} />
    case 'not-found':
      return (
        <div className="flex w-full max-w-2xl flex-col gap-3 text-left">
          <h2 className="text-xl font-semibold">Page not found</h2>
          <p className="text-slate-600 dark:text-slate-400">
            There is nothing at <code>{route.path}</code>.
          </p>
          <Link to="/" className="self-start font-medium underline">
            Go to the submit form
          </Link>
        </div>
      )
  }
}

export default App
