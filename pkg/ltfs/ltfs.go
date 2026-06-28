// Package ltfs wraps the reference LinearTape-Open LTFS tools (mkltfs, ltfs,
// fusermount) to format a blank tape, mount it as a FUSE filesystem with the
// index sync deferred to unmount, and capture the on-tape index. This is the
// core of the Write phase (SPEC.md §4.3 step 7) and directly serves principle 2
// (minimize tape wear, SPEC.md §14).
//
// # Device node
//
// The reference LTFS drives the tape through its "sg" backend, i.e. the SCSI
// generic node (/dev/sg0, /dev/sg1), not the non-rewinding st node (/dev/nst0)
// that mt/tar use. Both node families are passed through to the data-worker
// container (SPEC.md §4.1), and pkg/tape's blank check likewise controls the
// drive through the sg node, so this is consistent with the rest of the system.
// All device paths are injected so the same code targets both mhvtl virtual
// hardware and real LTO drives.
//
// # Tape wear
//
// Mount uses `-o sync_type=unmount`, so LTFS writes the index exactly once, at
// unmount, instead of repositioning mid-stream to sync it periodically — each
// such sync is a back-hitch and an extra write pass this single-write workflow
// has no reason to incur (SPEC.md §14). Mount also uses `-o capture_index`, so
// the index LTFS writes at unmount is dumped to the work directory as XML; it is
// byte-identical to the on-tape index and is read back by ReadIndex with no
// extra tape movement.
package ltfs

// Volume wraps a single tape drive's LTFS operations, addressed by its SCSI
// generic device node (the reference LTFS "sg" backend), e.g. /dev/sg0.
type Volume struct {
	// device is the SCSI generic node the LTFS sg backend operates on.
	device string
}

// NewVolume returns a Volume targeting the given SCSI generic device node
// (e.g. /dev/sg0).
func NewVolume(device string) *Volume {
	return &Volume{device: device}
}
