package tape

import (
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
)

// ltoDensityCodes maps a SCSI medium density code to the LTO generation of the
// medium carrying it. These are the LTO generations' assigned density codes; the
// values match mhvtl's own table (include/vtlcart.h), and 0x58 is corroborated by
// the Linux st driver, which reports an LTO-5 cartridge as "Density code 0x58
// (LTO-5)".
//
// LTO-8 media come in two densities: the LTO-7 Type M ("M8") initialization of an
// LTO-7 cartridge (0x5d) and native LTO-8 (0x5e). They are distinguished because
// they are not interchangeable to a recoverer — M8 media are readable only by
// LTO-8 drives.
var ltoDensityCodes = map[byte]string{
	0x40: "LTO-1",
	0x42: "LTO-2",
	0x44: "LTO-3",
	0x46: "LTO-4",
	0x58: "LTO-5",
	0x5a: "LTO-6",
	0x5c: "LTO-7",
	0x5d: "LTO-7 Type M (M8)",
	0x5e: "LTO-8",
	0x60: "LTO-9",
}

// MediumGeneration reports the LTO generation of the medium currently loaded in
// the drive at sgDevice (a SCSI generic node, e.g. /dev/sg1), read from the
// density code in the MODE SENSE block descriptor.
//
// This is the generation of the *tape*, which is what a recoverer needs a drive
// for — deliberately not the drive's own generation (DeviceInfo.LTOGeneration),
// which says only what wrote it. An LTO-6 drive writes LTO-5 media, and an LTO-8
// drive can read neither, so reporting the writing drive's generation would send a
// recoverer after the wrong hardware (issue #340).
//
// It must be called while the medium is loaded — an empty drive has no medium to
// describe. An unrecognized density code yields unknownGeneration rather than an
// error, so a medium this table does not know is still reported honestly.
func MediumGeneration(ctx context.Context, sgDevice string) (string, error) {
	// --raw emits the MODE SENSE response verbatim, so the density code is read
	// at its fixed offset in the SCSI structure. sg_modes' decoded text carries
	// the same value but its layout varies across sg3-utils releases, and unlike
	// sg_logs (see logpage.go) sg_modes has no --json output to parse instead.
	out, err := exec.CommandContext(ctx, "sg_modes", "--raw", sgDevice).Output()
	if err != nil {
		return unknownGeneration, fmt.Errorf("sg_modes --raw %s: %w", sgDevice, err)
	}

	code, ok := densityCode(out)
	if !ok {
		return unknownGeneration, fmt.Errorf("sg_modes --raw %s: response carries no block descriptor", sgDevice)
	}

	generation, known := ltoDensityCodes[code]
	if !known {
		return unknownGeneration, nil
	}

	return generation, nil
}

// densityCode extracts the medium density code from a MODE SENSE(10) response:
// an 8-byte mode parameter header whose last two bytes hold the block descriptor
// length, followed by the block descriptors themselves, the first of which opens
// with the density code. It reports false when the response is truncated or the
// drive returned no block descriptor (nothing loaded, or descriptors disabled),
// so a missing density is never mistaken for density code 0x00.
func densityCode(response []byte) (byte, bool) {
	const headerLen = 8

	if len(response) < headerLen {
		return 0, false
	}

	descriptorLen := binary.BigEndian.Uint16(response[6:headerLen])
	if descriptorLen == 0 || len(response) < headerLen+int(descriptorLen) {
		return 0, false
	}

	return response[headerLen], true
}
