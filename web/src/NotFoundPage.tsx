import { Link } from './router'

// NotFoundPage is the 404 view in the new design language (issue #272),
// replacing the previous ad hoc inline "not-found" render in App.tsx.
// Renders inside the authenticated shell's content area (Sidebar/header
// stay visible), same as the old inline version — an operator who mistypes
// a run ID or follows a stale link never loses their way back into the app.
function NotFoundPage({ path }: { path: string }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-4 px-6 py-16 text-center">
      <div className="font-mono text-[13px] tracking-[0.08em] text-text-faint">404</div>
      <h1 className="text-xl font-semibold tracking-tight">Page not found</h1>
      <p className="max-w-sm text-[12.5px] leading-relaxed text-text-dim">
        There is nothing at <code className="font-mono text-text">{path}</code>.
      </p>
      <Link
        to="/history"
        className="mt-2 rounded-[9px] bg-text px-4 py-2 text-[12.5px] font-semibold text-bg shadow-card transition-opacity hover:opacity-90"
      >
        Back to dashboard
      </Link>
    </div>
  )
}

export default NotFoundPage
