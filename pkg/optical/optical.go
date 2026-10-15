// Package optical burns, blanks, ejects, and verifies the optical recovery disc
// (SPEC §10) via the pinned xorriso binary.
//
// The overwrite decision (whether a non-blank disc may be reclaimed) lives in the
// workflow layer; this package only reports state and performs what it is asked.
//
// M-DISC is write-once DVD-R: an image cannot be dd'd, it needs MMC track/session
// finalization, hence the shell-out. xorriso's "stdio:" pseudo-drive runs the
// same burn path against a file or loop device, so it is testable without
// hardware. xorriso ships only in the data-worker image; the recovery disc just
// needs to read ISO 9660.
package optical

import "strings"

const xorrisoBin = "xorriso"

// Disc is an optical disc addressed by device path: a real drive (e.g.
// /dev/sr0) or, in tests, a file or loop device backing a pseudo-disc.
type Disc struct {
	device string
}

// NewDisc returns a Disc targeting the given device path.
func NewDisc(device string) *Disc {
	return &Disc{device: device}
}

// opticalDevicePrefixes denote real MMC drives; anything else goes through stdio:.
var opticalDevicePrefixes = []string{"/dev/sr", "/dev/scd", "/dev/dvd", "/dev/cdrom"}

func isOpticalDevice(device string) bool {
	for _, prefix := range opticalDevicePrefixes {
		if strings.HasPrefix(device, prefix) {
			return true
		}
	}

	return false
}

// driveAddress returns the xorriso drive address. stdio: media are always
// overwriteable, so write-once states cannot arise on them; tests for those
// states need real hardware.
func (d *Disc) driveAddress() string {
	if isOpticalDevice(d.device) {
		return d.device
	}

	return "stdio:" + d.device
}
