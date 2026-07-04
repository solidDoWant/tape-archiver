package tape

import "sync"

// Changer drives a SCSI media-changer (tape library robot) directly via SG_IO,
// issuing the two SCSI commands the library needs — READ ELEMENT STATUS (0xB8)
// for inventory and MOVE MEDIUM (0xA5) for load/unload/transfer — plus a one-time
// MODE SENSE of the Element Address Assignment page (0x1D) to map the library's
// raw element addresses to the friendly drive/slot numbers callers use. It
// replaces the former "mtx" subprocess so the import/export ACCESS bit, which the
// mtx text output discards, is available to the operator-in-the-loop Eject phase
// (SPEC §4.3 phase 8).
//
// The SG_IO decoding is Linux-only (changer_linux.go); other platforms get build
// stubs (changer_other.go). The address arithmetic and binary descriptor decoding
// are platform-independent (changer_decode.go).
type Changer struct {
	// device is the path passed to NewChanger. It is normally the changer node
	// (/dev/sch0); the paired SCSI generic node (/dev/sgN) is resolved from its
	// SCSI address on first use. A /dev/sgN path is also accepted and used as-is.
	device string
	// sgDevice, when set with WithChangerSGDevice, overrides the resolved SCSI
	// generic node.
	sgDevice string

	// mu guards the lazily-resolved, cached SG node and addressing map so a
	// Changer is safe to reuse across sequential activity calls.
	mu sync.Mutex
	// resolvedSG is the SCSI generic node actually issued commands, cached after
	// the first resolution.
	resolvedSG string
	// addressing is the raw↔friendly element-address map read once from mode page
	// 0x1D (the same authoritative source mtx reads) and cached.
	addressing *elementAddressing
}

// ChangerOption configures a Changer.
type ChangerOption func(*Changer)

// WithChangerSGDevice overrides the SCSI generic node (/dev/sgN) used to issue
// commands. By default the sg node is resolved from the changer device's SCSI
// address (see Inventory). Use this for non-standard device topologies or tests.
func WithChangerSGDevice(sgDevice string) ChangerOption {
	return func(c *Changer) {
		c.sgDevice = sgDevice
	}
}

// NewChanger returns a Changer targeting the given device. Pass the changer node
// (e.g. /dev/sch0); its paired SCSI generic node is resolved from the SCSI
// address on first use. A /dev/sgN node is also accepted and used directly, and
// WithChangerSGDevice overrides the resolution entirely.
func NewChanger(device string, opts ...ChangerOption) *Changer {
	c := &Changer{device: device}

	for _, opt := range opts {
		opt(c)
	}

	return c
}
