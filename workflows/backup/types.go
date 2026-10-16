package backup

import (
	"time"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// ResolvedArchive is one config Source resolved to the ZFS snapshot(s) that
// become a single archive (SPEC §4.3 phase 1, §5).
type ResolvedArchive struct {
	// SourceIndex is the originating Source's position in Config.Sources.
	SourceIndex int
	// Label names the archive's on-tape directory (SPEC §6). It is never relied on
	// for uniqueness; the SourceIndex prefix disambiguates.
	Label string
	// Compression is whether Prepare zstd-compresses this archive.
	Compression bool
	// Snapshots make up this archive, in resolution order.
	Snapshots []ResolvedSnapshot
	// EstimatedBytes is the inflated feasibility estimate, used only to reject an
	// archive that cannot fit on one tape; the measured Prepare size is the plan.
	EstimatedBytes int64
	// LogicalBytes is the summed logicalreferenced before inflation; Prepare
	// reports progress against it.
	LogicalBytes int64
}

// ResolvedSnapshot is one ZFS snapshot within a ResolvedArchive, with its
// provenance.
type ResolvedSnapshot struct {
	// ZFSPath is the absolute snapshot path, e.g. bulk-pool-01/archive@daily.
	ZFSPath string
	// Dataset and SnapshotName are set for k8s sources (for k8ssnap.Verify) and
	// run-scoped snapshots; empty for other raw ZFS paths.
	Dataset      string
	SnapshotName string
	// Namespace, VolumeSnapshot, and PVC are the k8s provenance for the report.
	Namespace      string
	VolumeSnapshot string
	PVC            string
	// RunScoped marks a snapshot of a live dataset taken by this run and
	// destroyed when it ends; CreatedAt is its creation time once taken.
	RunScoped bool
	CreatedAt time.Time
}

// StagedArchive is an archive staged to disk by Prepare (SPEC §4.3 phase 2).
// Its SizeBytes is the authoritative size Pack bin-packs by.
type StagedArchive struct {
	SourceIndex int
	// Slices concatenate to the exact age stream.
	Slices    []StagedSlice
	SizeBytes int64
}

// StagedSlice is one staged file on disk with its SHA-256 and size.
type StagedSlice struct {
	Path string
	// SHA256 is the lowercase hex digest.
	SHA256    string
	SizeBytes int64
}

// TapePlan is the Pack phase's assignment of staged archives to logical tapes
// (SPEC §4.3 phase 3).
type TapePlan struct {
	// Copies is the number of byte-identical physical copies per logical tape.
	// PAR2 is planned once per logical tape, so it does not depend on Copies.
	Copies int
	Tapes  []PlannedTape
}

// PlannedTape is one logical tape in the Pack plan.
type PlannedTape struct {
	Archives []PlannedArchive
	// UsableBytes is native capacity less reserved LTFS overhead; fill-mode PAR2
	// grows into the slack above PlannedBytes.
	UsableBytes int64
}

// PlannedBytes is the tape's data plus reserved PAR2; Pack keeps it within
// UsableBytes.
func (t PlannedTape) PlannedBytes() int64 {
	var total int64
	for _, archive := range t.Archives {
		total += archive.Footprint()
	}

	return total
}

// DataBytes is the tape's archive data excluding PAR2.
func (t PlannedTape) DataBytes() int64 {
	var total int64
	for _, archive := range t.Archives {
		total += archive.DataBytes
	}

	return total
}

// PlannedArchive is one staged archive's placement on a tape.
type PlannedArchive struct {
	SourceIndex int
	DataBytes   int64
	// SliceCount is carried so fill mode can recompute par2.MaxOutputBytes, which
	// depends on the file count.
	SliceCount int
	// PAR2ReservedBytes is the minimum PAR2 reserved at pack time; fill mode may
	// grow the real set into remaining capacity.
	PAR2ReservedBytes int64
}

// Footprint is the archive's data plus reserved PAR2.
func (p PlannedArchive) Footprint() int64 {
	return p.DataBytes + p.PAR2ReservedBytes
}

// PAR2Set is one archive's staged PAR2 recovery set (SPEC §4.3 phase 4).
type PAR2Set struct {
	SourceIndex       int
	RedundancyPercent int
	// Files are the index and volume files, sorted by name.
	Files []StagedSlice
}

// VerifiedPlan marks a TapePlan whose staged tree passed Verify (SPEC §4.3
// phase 5); no tape is written without one.
type VerifiedPlan struct{}

// TapeAssignment is one physical tape's placement within a drive-set: its
// drive, blank slot, and (logical tape, copy) pair.
type TapeAssignment struct {
	// Drive is the non-rewinding device node. Load pairs it to the changer's
	// element by unit serial, not set position, so non-prefix sets and mismatched
	// kernel probe order still use this exact drive (issue #137).
	Drive string
	// DriveIndex is Drive's index in cfg.Library.Drives.
	DriveIndex int
	// BlankSlot is unique per physical tape across the run.
	BlankSlot int
	TapeIndex int
	CopyIndex int
}

// driveSet is the batch of at most len(Drives) tapes written in parallel; each
// set is loaded, written, and ejected before the next.
type driveSet []TapeAssignment

// LoadedTape is one physical tape loaded into a drive (SPEC §4.3 phase 6).
type LoadedTape struct {
	// Barcode is the volume tag, used as the LTFS volume name (SPEC §6).
	Barcode tape.Barcode
	// DriveIndex comes from the TapeAssignment, not set position (issue #137).
	DriveIndex int
	TapeIndex  int
	CopyIndex  int
	// SourceSlot is where Eject returns the tape before exporting it.
	SourceSlot int
	STDevice   string
	SGDevice   string
	// OverwroteNonBlank is set when a non-blank tape was written under
	// Library.AllowNonBlankTapes, so the report records it (SPEC §9).
	OverwroteNonBlank bool
}

// WrittenTape records a tape written during the Write phase, for Eject and the
// report.
type WrittenTape struct {
	Barcode    tape.Barcode
	DriveIndex int
	TapeIndex  int
	CopyIndex  int
	SourceSlot int
	// IndexXMLPath is the staged copy of the LTFS index written at unmount. A
	// path, not bytes, so the multi-MB index never rides an activity payload
	// (issue #221).
	IndexXMLPath string
	// WriteHealth is observational only and never affects run success (SPEC §14).
	WriteHealth WriteHealth
	// MediaGeneration is the medium's LTO generation, or empty/"unknown" if it
	// could not be read (issue #340).
	MediaGeneration   string
	OverwroteNonBlank bool
}

// EjectResult is one Eject call's outcome. Tapes that did not fit the I/O
// station are returned instead of failing, so the workflow can pause.
type EjectResult struct {
	// InIOStation is every run tape now in the I/O station, across all calls.
	InIOStation []tape.Barcode
	// Remaining are tapes back in their source slots awaiting a free I/O slot;
	// empty means Eject is complete.
	Remaining []WrittenTape
}

// IOStatus is an I/O-station snapshot used to auto-resume a paused Eject.
type IOStatus struct {
	FreeSlots int
	// AccessReported is false when the library does not report the ACCESS bit;
	// the workflow then waits for an explicit operator signal.
	AccessReported bool
	// StationClosed is only meaningful when AccessReported is true.
	StationClosed bool
}

// CanAutoResume reports whether a paused Eject may resume without an operator
// signal (SPEC §4.3 phase 8 AC2).
func (s IOStatus) CanAutoResume() bool {
	return s.AccessReported && s.StationClosed && s.FreeSlots > 0
}

// Result is the backup workflow's success value.
type Result struct {
	// CompletedPhases lists completed phases in execution order.
	CompletedPhases []string
}

// PauseKind identifies the active operator pause for CurrentPauseQuery;
// PauseNone means not paused.
type PauseKind string

const (
	PauseNone PauseKind = ""
	// PauseEject: the I/O station filled. Only resume applies — every tape is
	// already written, so waitForIOCleared never listens for abort.
	PauseEject PauseKind = "eject"
	// PauseWriteFailure: a Load or Write failed for a drive-set. Resume or abort.
	PauseWriteFailure PauseKind = "write-failure"
	// PauseBurn: a burn/verify failure or between-set disc swap. Resume or abort.
	PauseBurn PauseKind = "burn"
)

// CurrentPause is the CurrentPauseQuery result: the active pause and enough
// context to act on it. All fields are zero when not paused.
type CurrentPause struct {
	Kind PauseKind
	// Phase is "Load" or "Write" for a write-failure pause.
	Phase string
	// AffectedTapes are tapes to replace (write failure) or remove from the I/O
	// station (eject).
	AffectedTapes []string
	// ReloadSlots are slots to restock with blanks (write failure).
	ReloadSlots []int
	// AwaitingExport counts tapes still to export (eject).
	AwaitingExport int
	// Devices are burners needing a fresh blank disc (burn).
	Devices []string
	// ErrorSummary is the failure text; empty for eject and disc-swap pauses.
	ErrorSummary string
}

// runState carries phase outputs to later phases, mutated in deterministic
// workflow order.
type runState struct {
	// lastCompletedPhase backs LastCompletedPhaseQuery.
	lastCompletedPhase string
	// currentPause backs CurrentPauseQuery; set after a pause's alert dispatches
	// and cleared when its wait returns.
	currentPause CurrentPause
	resolved     []ResolvedArchive
	// sources is the config's sources with each zfsProperty source expanded
	// into one zfsPath source per matched dataset; nil when none were expanded.
	sources []config.Source
	// sliceSizeBytes is derived by DeriveSliceSize at the end of Resolve, not
	// configured (issue #324).
	sliceSizeBytes int64
	staged         []StagedArchive
	plan           TapePlan
	par2           []PAR2Set
	// verified is required before any tape is touched.
	verified VerifiedPlan
	// loaded is the in-flight drive-set only.
	loaded []LoadedTape
	// mountedBarcodes is every tape the run mounted: a tape that later failed to
	// write still left staging entries that cleanup must remove.
	mountedBarcodes []tape.Barcode
	written         []WrittenTape
	reportPath      string
	// uncompressedISOPath and discManifestPath are set only when optical burning
	// is enabled.
	uncompressedISOPath string
	discManifestPath    string
	// reportDate is captured once so the post-burn report re-render keeps the
	// on-disc copy's date (SPEC §10).
	reportDate  time.Time
	burnedDiscs []BurnResult
}
