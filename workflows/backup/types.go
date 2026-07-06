package backup

import (
	"time"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

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
// exact on-disk size measured and a SHA-256 checksum per slice. Its SizeBytes is
// the authoritative size the Pack phase bin-packs by — the measured staged size,
// not the Resolve estimate (SPEC §4.3 phase 3).
type StagedArchive struct {
	// SourceIndex ties the staged archive back to its originating Source in
	// Config.Sources, matching the ResolvedArchive it was prepared from.
	SourceIndex int
	// Slices are the staged slice files in order; their concatenation
	// reconstructs the age stream exactly. Each carries its own SHA-256 and size.
	Slices []StagedSlice
	// SizeBytes is the measured total on-disk size across all slices — the
	// authoritative size for the Pack phase (SPEC §4.3 phases 2–3).
	SizeBytes int64
}

// StagedSlice is one fixed-size slice file produced by the Prepare phase's split
// stage, with the SHA-256 the Verify and Write phases re-check it against.
type StagedSlice struct {
	// Path is the absolute path of the staged slice file on disk.
	Path string
	// SHA256 is the lowercase hex SHA-256 digest of the slice file's contents.
	SHA256 string
	// SizeBytes is the slice file's size on disk in bytes.
	SizeBytes int64
}

// TapePlan is the assignment of staged archives to tapes produced by the Pack
// phase (SPEC §4.3 phase 3): a bin-packing of archives onto tapes by measured
// size, within capacity, replicated across the configured number of copies.
type TapePlan struct {
	// Copies is the number of identical physical copies each logical tape is
	// written to — the configured copy count, one per drive for parallel
	// writing (SPEC §4.3 phase 3). The copies are byte-identical, so the staged
	// tree (slices + PAR2) is planned and generated once per logical tape and
	// written to all copies; PAR2 sizing therefore depends on the logical plan,
	// not the copy count.
	Copies int
	// Tapes are the logical tapes the archives are bin-packed onto, in plan
	// order. Each is materialized Copies times at write time.
	Tapes []PlannedTape
}

// PlannedTape is one logical tape in the Pack plan: the staged archives
// bin-packed onto it and the capacity accounting that keeps its contents within
// a single tape.
type PlannedTape struct {
	// Archives are the staged archives assigned to this tape, in source order.
	// Each references its StagedArchive by SourceIndex.
	Archives []PlannedArchive
	// UsableBytes is the tape's native capacity less the reserved LTFS
	// filesystem overhead — the ceiling the archives' data plus PAR2 must fit
	// within (SPEC §4.3 phase 3). The fill-to-capacity PAR2 phase grows recovery
	// sets into the slack between PlannedBytes and UsableBytes.
	UsableBytes int64
}

// PlannedBytes is the tape's total reserved footprint: every assigned archive's
// data plus its reserved PAR2. The Pack invariant is PlannedBytes ≤ UsableBytes.
func (t PlannedTape) PlannedBytes() int64 {
	var total int64
	for _, archive := range t.Archives {
		total += archive.Footprint()
	}

	return total
}

// DataBytes is the total measured archive data on the tape, excluding PAR2 — the
// base the fill-to-capacity PAR2 percentage is computed against.
func (t PlannedTape) DataBytes() int64 {
	var total int64
	for _, archive := range t.Archives {
		total += archive.DataBytes
	}

	return total
}

// PlannedArchive is one staged archive's placement on a tape: its measured data
// size and the PAR2 recovery bytes reserved for it during packing.
type PlannedArchive struct {
	// SourceIndex ties the placement back to its StagedArchive (and the
	// originating config Source).
	SourceIndex int
	// DataBytes is the archive's measured staged size (StagedArchive.SizeBytes).
	DataBytes int64
	// PAR2ReservedBytes is the PAR2 footprint reserved for the archive during
	// packing — the minimum recovery set size at the fixed target percentage or
	// the fill-to-capacity floor. The Generate PAR2 phase may grow the actual set
	// up to the tape's remaining capacity in fill mode (SPEC §4.3 phases 3–4).
	PAR2ReservedBytes int64
}

// Footprint is the archive's total reserved size on a tape: its measured data
// plus its reserved PAR2.
func (p PlannedArchive) Footprint() int64 {
	return p.DataBytes + p.PAR2ReservedBytes
}

// PAR2Set is the per-archive PAR2 recovery set generated and staged by the
// Generate PAR2 phase (SPEC §4.3 phase 4): the recovery files written alongside
// the archive's slices, each checksummed, at the chosen redundancy percentage.
type PAR2Set struct {
	// SourceIndex ties the recovery set back to its StagedArchive.
	SourceIndex int
	// RedundancyPercent is the PAR2 redundancy percentage the set was generated
	// at: the fixed target, or the per-tape fill-to-capacity percentage.
	RedundancyPercent int
	// Files are the staged PAR2 recovery files (the index plus its volume files),
	// each with its SHA-256 and on-disk size, in sorted name order. They reuse
	// StagedSlice — a staged, checksummed file on disk — since that is exactly
	// their shape.
	Files []StagedSlice
}

// VerifiedPlan is a TapePlan whose complete staged tree has passed checksum and
// capacity verification in the Verify phase (SPEC §4.3 phase 5). A run cannot
// proceed to write without one. Later sub-issues add the verification results.
type VerifiedPlan struct{}

// TapeAssignment is one physical tape's placement within a drive-set (SPEC §4.3
// phases 6–8): which drive writes it, which blank slot holds it, and which
// (logical tape, copy) pair it materializes. planDriveSets produces these; the
// Load activity turns each into a LoadedTape.
type TapeAssignment struct {
	// Drive is the non-rewinding tape device node (e.g. /dev/nst0) this physical
	// tape is loaded into and written on. Within a drive-set the assignments map
	// one-to-one onto the library's drives in order.
	Drive string
	// BlankSlot is the storage slot address holding the blank tape to load. Every
	// physical tape across the whole run has its own blank slot.
	BlankSlot int
	// TapeIndex is the 0-based logical tape (an index into plan.Tapes) this
	// physical tape carries. All copies of a logical tape share it.
	TapeIndex int
	// CopyIndex is the 0-based copy number (0..plan.Copies-1) of this physical tape.
	CopyIndex int
}

// driveSet is the batch of physical tapes written in parallel in one pass of the
// tape path — at most len(Drives) of them (SPEC §4.3 phases 6–8). A run spanning
// more physical tapes than the library has drives is written as a sequence of
// drive-sets, each loaded, written, and ejected before the next begins.
type driveSet []TapeAssignment

// LoadedTape is one physical tape loaded into a drive by the Load phase
// (SPEC §4.3 phase 6). It carries the device and slot provenance the Write and
// Eject phases need to format, mount, and return the tape.
type LoadedTape struct {
	// Barcode is the tape's library barcode (the SCSI volume tag), used as the
	// LTFS volume name and as the manifest and report identity (SPEC §6).
	Barcode tape.Barcode
	// DriveIndex is the 0-based position of the drive in cfg.Library.Drives,
	// tying the tape to its physical drive for Write and Eject.
	DriveIndex int
	// TapeIndex is the 0-based index of the logical tape this physical tape
	// carries (an index into plan.Tapes). All copies of the same logical tape
	// share the same TapeIndex.
	TapeIndex int
	// CopyIndex is the 0-based copy number (0..plan.Copies-1) among the
	// copies of this logical tape.
	CopyIndex int
	// SourceSlot is the storage slot address the blank tape was loaded from.
	// The Eject phase unloads the tape back to this slot before transferring
	// it to an I/O station.
	SourceSlot int
	// STDevice is the non-rewinding tape device node (e.g. /dev/nst0).
	STDevice string
	// SGDevice is the SCSI generic device node (e.g. /dev/sg1) used by LTFS
	// and the FormatTape activity (the reference LTFS sg backend).
	SGDevice string
	// OverwroteNonBlank is true when this tape was found to be non-blank at load
	// and written anyway because the run set Library.AllowNonBlankTapes. It is
	// false for the normal (blank) path. The Write phase carries it onto the
	// WrittenTape so the run report can record the deliberate overwrite (SPEC §9).
	OverwroteNonBlank bool
}

// WrittenTape records a tape written during the Write phase (SPEC §4.3 phase 7).
// It carries all state needed by the Eject phase and the run report.
type WrittenTape struct {
	// Barcode is the tape's library barcode, its canonical identity (SPEC §6).
	Barcode tape.Barcode
	// DriveIndex is the 0-based drive index within cfg.Library.Drives.
	DriveIndex int
	// TapeIndex is the 0-based logical-tape index within plan.Tapes.
	TapeIndex int
	// CopyIndex is the 0-based copy number among the copies of this logical tape.
	CopyIndex int
	// SourceSlot is the storage slot the blank tape was loaded from; the Eject
	// phase unloads the tape to this slot before transferring it to I/O.
	SourceSlot int
	// IndexXML is the captured LTFS index returned by FinalizeTape, byte-identical
	// to the index LTFS wrote to the tape's index partition at unmount (SPEC §6,
	// §10). It is included in the recovery ISO.
	IndexXML []byte
	// WriteHealth is the observational write-health measurement for this tape
	// (sustained throughput, repositions, TapeAlert flags), taken after the write
	// window closed. It never affects run success (SPEC §2 principle 2, §14).
	WriteHealth WriteHealth
	// OverwroteNonBlank is true when this tape was non-blank at load and written
	// over because the run set Library.AllowNonBlankTapes. The run report records
	// it so the deliberate overwrite is observable (SPEC §9).
	OverwroteNonBlank bool
}

// EjectResult reports the outcome of one Eject activity call (SPEC §4.3 phase 8).
// The Eject activity exports as many tapes as the free I/O-station slots hold and,
// rather than failing when the station fills, returns the tapes it could not yet
// export so the workflow can pause for the operator and resume.
type EjectResult struct {
	// InIOStation lists the barcodes of this run's written tapes currently sitting
	// in I/O-station slots — the tapes ready for the operator to remove. It is the
	// full set across every Eject call so far, not just this call's transfers.
	InIOStation []tape.Barcode
	// Remaining lists the written tapes not yet exported. Each has been unloaded
	// from its drive to its source storage slot (so no tape is left in a drive),
	// and awaits a free I/O slot. Empty means the Eject phase is complete.
	Remaining []WrittenTape
}

// IOStatus is a read-only snapshot of the import/export station used to decide
// whether a paused Eject phase can resume automatically (SPEC §4.3 phase 8).
type IOStatus struct {
	// FreeSlots is the number of empty I/O-station slots.
	FreeSlots int
	// AccessReported is true when the library annotates the import/export ACCESS
	// bit (tape.Inventory.IOAccessReported). When false the door cycle cannot be
	// detected and the workflow waits for the explicit operator signal instead.
	AccessReported bool
	// StationClosed is true when every I/O-station slot reports accessible to the
	// changer robot — the operator has closed the station. It is only meaningful
	// when AccessReported is true.
	StationClosed bool
}

// CanAutoResume reports whether a paused Eject phase may resume without an
// explicit operator signal: the library reports access state, the station is
// closed, and at least one I/O slot is free (SPEC §4.3 phase 8 AC2).
func (s IOStatus) CanAutoResume() bool {
	return s.AccessReported && s.StationClosed && s.FreeSlots > 0
}

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
	// staged is the prepared work list the Prepare phase produces (SPEC §4.3
	// phase 2): each resolved archive tarred, optionally compressed, encrypted,
	// sliced, and checksummed on disk, with its measured size. The Pack phase
	// consumes it.
	staged []StagedArchive
	// plan is the bin-packing the Pack phase produces (SPEC §4.3 phase 3): the
	// staged archives assigned to logical tapes within capacity, replicated
	// across the configured copies. The Generate PAR2 and later phases consume it.
	plan TapePlan
	// par2 is the per-archive PAR2 recovery sets the Generate PAR2 phase produces
	// (SPEC §4.3 phase 4), staged and checksummed alongside the slices. The
	// Verify and Write phases consume it.
	par2 []PAR2Set
	// verified is the plan the Verify phase produces (SPEC §4.3 phase 5): the
	// Pack plan once its complete staged tree has passed checksum and capacity
	// verification on disk. The Load phase requires it before any tape is touched.
	verified VerifiedPlan
	// loaded holds the tapes the current drive-set loaded into drives (SPEC §4.3
	// phase 6). The tape path processes drive-sets one at a time, so this is the
	// in-flight set (at most len(Drives) tapes), overwritten as each set loads.
	loaded []LoadedTape
	// written accumulates every tape written by the Write phase across all
	// drive-sets (SPEC §4.3 phase 7). Each set's tapes are appended as it
	// completes; the Report phase uses the full list (barcodes, IndexXML) for the
	// report and recovery ISO.
	written []WrittenTape
	// reportPath and isoPath are the on-disk paths of the artifacts the Report
	// phase builds (SPEC §4.3 phase 9): the PDF report and the compressed recovery
	// ISO, staged on the data worker. The Deliver phase uploads them.
	reportPath string
	isoPath    string
	// uncompressedISOPath is the on-disk path of the uncompressed recovery ISO 9660
	// image the Report phase stages only when optical burning is enabled
	// (delivery.opticalBurn). It is the mountable image the Burn phase consumes;
	// empty when burning is disabled.
	uncompressedISOPath string
	// discManifestPath is the on-disk sha256sum manifest of the recovery ISO's
	// contents, staged by the Report phase only when optical burning is enabled.
	// The Burn phase passes it to VerifyDisc so each burned disc is read back and
	// checked against it (SPEC §10). Empty when burning is disabled.
	discManifestPath string
	// reportDate is the run's report timestamp (workflow.Now at the Report phase),
	// captured once so the post-burn re-render of the delivered report carries the
	// same date as the on-disc copy that predates the burn (SPEC §10).
	reportDate time.Time
	// burnedDiscs accumulates every recovery disc the Burn phase burned and
	// verified, across all burn-sets (SPEC §4.3, §10). The post-burn report
	// re-render records them (including any deliberate overwrite). Empty when
	// optical burning is disabled.
	burnedDiscs []BurnResult
}
