package tape

// Drive wraps a tape drive addressed by its non-rewinding st node (data path;
// opening and closing it never repositions the tape) and its sg node (SG_IO
// control commands).
type Drive struct {
	stDevice string
	sgDevice string
}

// DriveOption configures a Drive.
type DriveOption func(*Drive)

// WithSGDevice overrides the sg node otherwise resolved from the tape device's
// SCSI address.
func WithSGDevice(sgDevice string) DriveOption {
	return func(d *Drive) {
		d.sgDevice = sgDevice
	}
}

// NewDrive returns a Drive for a non-rewinding tape device (e.g. /dev/nst0).
func NewDrive(device string, opts ...DriveOption) *Drive {
	d := &Drive{stDevice: device}

	for _, opt := range opts {
		opt(d)
	}

	return d
}

// STDevice returns the non-rewinding tape device node (e.g. /dev/nst0).
func (d *Drive) STDevice() string {
	return d.stDevice
}
