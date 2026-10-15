package tape

import (
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
)

// ltoDensityCodes maps a medium density code to its LTO generation (values
// match mhvtl's include/vtlcart.h). M8 is kept distinct from LTO-8 because only
// LTO-8 drives can read it.
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

// MediumGeneration reports the LTO generation of the medium loaded in the
// drive at sgDevice, from the MODE SENSE density code.
//
// It reports the tape's generation, not the drive's: a recoverer needs a drive
// that reads the medium, and an LTO-6 drive writing LTO-5 media would otherwise
// send them after the wrong hardware (#340). TEST UNIT READY comes first
// because mhvtl keeps reporting the last medium's density after an unload.
func MediumGeneration(ctx context.Context, sgDevice string) (string, error) {
	if err := testUnitReady(ctx, sgDevice); err != nil {
		return unknownGeneration, fmt.Errorf("drive %s has no ready medium: %w", sgDevice, err)
	}

	// sg_modes' decoded text varies across sg3-utils releases and it has no
	// --json, so read the density at its fixed offset in the raw response.
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

// densityCode extracts the density code from a MODE SENSE(10) response,
// reporting false when there is no block descriptor so a missing density is
// never mistaken for code 0x00.
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
