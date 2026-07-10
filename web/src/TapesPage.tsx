// TapesPage is a minimal placeholder for the sidebar's "Tapes" nav item
// (issue #272's app-shell acceptance criterion requires the nav item to
// exist). The real Tapes page — live library contents read from the
// changer, plus history-derived exported-tape records — is a separate,
// later issue (DESIGN_ANALYSIS.md §2/§7); this exists only so the nav item
// routes somewhere real instead of a dead link, without redesigning content
// this issue's non-goals explicitly defer.
function TapesPage() {
  return (
    <div className="flex max-w-[720px] flex-col gap-4 p-6 sm:p-7">
      <div>
        <h1 className="text-lg font-semibold tracking-tight">Tapes</h1>
        <p className="mt-1 text-[12.5px] text-text-dim">
          Live library contents and exported-tape history will live here.
        </p>
      </div>
      <div className="rounded-xl border border-dashed border-border-strong bg-surface p-6 text-center text-[12.5px] text-text-faint">
        This page is under construction.
      </div>
    </div>
  )
}

export default TapesPage
