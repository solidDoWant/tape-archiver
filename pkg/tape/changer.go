package tape

import "sync"

// Changer drives a tape library robot directly via SG_IO (READ ELEMENT STATUS
// and MOVE MEDIUM). It replaced mtx, whose text output drops the I/O-station
// ACCESS bit the Eject phase needs (SPEC §4.3 phase 8).
type Changer struct {
	// device is normally /dev/schN; a /dev/sgN path is used as-is.
	device   string
	sgDevice string

	// mu guards the lazily resolved sg node and addressing map.
	mu         sync.Mutex
	resolvedSG string
	addressing *elementAddressing
}

// ChangerOption configures a Changer.
type ChangerOption func(*Changer)

// WithChangerSGDevice overrides the sg node otherwise resolved from the
// changer's SCSI address.
func WithChangerSGDevice(sgDevice string) ChangerOption {
	return func(c *Changer) {
		c.sgDevice = sgDevice
	}
}

// NewChanger returns a Changer for a changer node (e.g. /dev/sch0) or a
// /dev/sgN node.
func NewChanger(device string, opts ...ChangerOption) *Changer {
	c := &Changer{device: device}

	for _, opt := range opts {
		opt(c)
	}

	return c
}
