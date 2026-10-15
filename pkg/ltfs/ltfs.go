// Package ltfs wraps the reference LTFS tools (mkltfs, ltfs, fusermount) to
// format a blank tape, mount it with the index sync deferred to unmount, and
// capture the on-tape index (SPEC.md §4.3 step 7, §14).
//
// LTFS drives the tape through its "sg" backend, so devices are SCSI generic
// nodes (/dev/sgN), not st nodes. `-o sync_type=unmount` writes the index once,
// at unmount, avoiding mid-stream back-hitches that cause tape wear.
package ltfs

// Volume wraps a single tape drive's LTFS operations via its sg device node.
type Volume struct {
	device string
}

// NewVolume returns a Volume targeting the given SCSI generic device node.
func NewVolume(device string) *Volume {
	return &Volume{device: device}
}
