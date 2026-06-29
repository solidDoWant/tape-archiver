package backup

// This file holds the run-state types that flow between the backup workflow's
// phases (SPEC §4.3). They are defined here as the scaffold so that each phase
// sub-issue is purely additive: a sub-issue fills in the fields its phase
// produces without restructuring the others. Until then they are intentionally
// sparse placeholders.

// ResolvedArchive is a single config Source resolved by the Resolve phase to a
// concrete ZFS snapshot to archive (SPEC §4.3 phase 1). Later sub-issues add the
// resolved snapshot path, ownership cross-check, and feasibility estimate.
type ResolvedArchive struct{}

// StagedArchive is a prepared archive staged to disk by the Prepare phase
// (SPEC §4.3 phase 2): tar → optional zstd → age → fixed-size slices, with the
// exact on-disk size measured and a SHA-256 checksum per slice. Later sub-issues
// add the measured size and per-slice checksum fields.
type StagedArchive struct{}

// TapePlan is the assignment of staged archives to tapes produced by the Pack
// phase (SPEC §4.3 phase 3): a bin-packing of archives onto tapes by measured
// size, within capacity, replicated across the configured number of copies.
// Later sub-issues add the per-tape assignments and capacity accounting.
type TapePlan struct{}

// PAR2Set is the per-archive PAR2 recovery set generated and staged by the
// Generate PAR2 phase (SPEC §4.3 phase 4). Later sub-issues add the recovery
// file set, redundancy percentage, and block size.
type PAR2Set struct{}

// VerifiedPlan is a TapePlan whose complete staged tree has passed checksum and
// capacity verification in the Verify phase (SPEC §4.3 phase 5). A run cannot
// proceed to write without one. Later sub-issues add the verification results.
type VerifiedPlan struct{}

// WrittenTape records a tape written during the Write phase (SPEC §4.3 phase 7),
// including any state that needs manual handling if the run later fails. Later
// sub-issues add the barcode, drive, captured LTFS index, and manifest.
type WrittenTape struct{}

// Result is the backup workflow's success return value. For now it reports the
// phases that ran to completion, in order; later sub-issues enrich it with a
// run summary (tapes written, sizes, checksums) for the report.
type Result struct {
	// CompletedPhases lists the phases that ran to completion, in execution
	// order. On success it contains all phases from Resolve through Deliver.
	CompletedPhases []string
}

// runState carries data produced by each phase to the phases that follow it,
// plus the progress marker the lastCompletedPhase query reads. It is mutated in
// workflow (single-threaded, deterministic) order as phases complete. The typed
// run-state fields above are threaded through here by their respective phase
// sub-issues; today only the progress marker is populated.
type runState struct {
	// lastCompletedPhase is the name of the most recently completed phase, or
	// the empty string before any phase has completed. Read by the
	// LastCompletedPhaseQuery handler.
	lastCompletedPhase string
}
