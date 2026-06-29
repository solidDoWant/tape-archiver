package backup

// This file holds the run-state types that flow between the backup workflow's
// phases (SPEC §4.3). They are defined here as the scaffold so that each phase
// sub-issue is purely additive: a sub-issue fills in the fields its phase
// produces without restructuring the others. Until then they are intentionally
// sparse placeholders.

// ResolvedArchive is a single config Source resolved by the Resolve phase to the
// concrete ZFS snapshot(s) that become one archive (SPEC §4.3 phase 1). A k8s
// single-snapshot source or a raw ZFS path yields one Snapshot; a k8s
// label-selector group yields one member per matched snapshot, all packed into
// the same tar (SPEC §5).
type ResolvedArchive struct {
	// SourceIndex is the position of the originating Source in Config.Sources,
	// preserved so the resolved work list stays aligned with the run config.
	SourceIndex int
	// Compression is whether the Prepare phase should zstd-compress this archive,
	// resolved from the source's optional override (default-on, SPEC §8).
	Compression bool
	// Snapshots are the ZFS snapshots that make up this archive, in resolution
	// order. A group has more than one; a single source has exactly one.
	Snapshots []ResolvedSnapshot
	// EstimatedBytes is the Resolve feasibility estimate: the summed
	// logicalreferenced of Snapshots inflated by the overhead factor and PAR2 %
	// (SPEC §4.3 phase 1). It exists only to reject an archive that cannot fit on
	// one tape before real work; it is never used as the authoritative plan, which
	// is the measured Prepare size.
	EstimatedBytes int64
}

// ResolvedSnapshot is one ZFS snapshot within a ResolvedArchive, with the
// provenance needed to verify and tar it. ZFSPath is the canonical
// "dataset@snapshot" the Prepare phase tars and the feasibility pre-check sizes.
type ResolvedSnapshot struct {
	// ZFSPath is the absolute ZFS snapshot path, e.g. bulk-pool-01/archive@daily.
	ZFSPath string
	// Dataset is the ZFS dataset (k8s sources); empty for raw ZFS paths.
	Dataset string
	// SnapshotName is the ZFS snapshot short name (k8s sources); empty for raw
	// ZFS paths. With Dataset it reconstructs the snapshot for ownership
	// verification (k8ssnap.Verify).
	SnapshotName string
	// Namespace, VolumeSnapshot, and PVC carry the k8s provenance of a resolved
	// VolumeSnapshot for the report; all empty for raw ZFS sources.
	Namespace      string
	VolumeSnapshot string
	PVC            string
}

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
	// resolved is the concrete work list the Resolve phase produces (SPEC §4.3
	// phase 1): every config Source expanded to its ZFS snapshot(s), verified and
	// feasibility-checked. The Prepare phase consumes it.
	resolved []ResolvedArchive
}
