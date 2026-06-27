package tape

// Drive wraps a single physical tape drive for drive-level operations.
//
// A drive is addressed through two device nodes that refer to the same unit:
//
//   - device:   the non-rewinding st node (e.g. /dev/nst0), used for the
//     streaming data path (writes/reads of archive data).
//   - sgDevice: the SCSI generic node (e.g. /dev/sg1), used for control
//     commands issued directly via SG_IO — currently the blank check (see
//     IsBlank). When not set explicitly it is resolved from the tape node's
//     SCSI address on first use.
//
// Always use the non-rewinding device node for the data path so that opening
// and closing it never repositions the tape (SPEC.md §4.3, "Hardware and
// Safety").
type Drive struct {
	device   string
	sgDevice string
}

// DriveOption configures a Drive.
type DriveOption func(*Drive)

// WithSGDevice overrides the SCSI generic node used for control commands (the
// blank check). By default the sg node is resolved from the tape device's SCSI
// address (see IsBlank). Use this for non-standard device topologies or tests.
func WithSGDevice(sgDevice string) DriveOption {
	return func(d *Drive) {
		d.sgDevice = sgDevice
	}
}

// NewDrive returns a Drive for the given non-rewinding tape device (e.g.
// /dev/nst0). The paired SCSI generic node used for the blank check is resolved
// from the tape node's SCSI address unless overridden with WithSGDevice.
func NewDrive(device string, opts ...DriveOption) *Drive {
	d := &Drive{device: device}

	for _, opt := range opts {
		opt(d)
	}

	return d
}
