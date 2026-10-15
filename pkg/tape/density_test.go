package tape

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// modeSenseResponse builds a MODE SENSE(10) response with one block descriptor.
func modeSenseResponse(density byte) []byte {
	return []byte{
		0x00, 0xea, // mode data length
		0x58,       // medium type
		0x10,       // device-specific parameter
		0x00, 0x00, // reserved / longlba
		0x00, 0x08, // block descriptor length
		density,          // block descriptor: density code
		0x00, 0x00, 0x00, // number of blocks
		0x00, 0x00, 0x00, 0x00, // reserved + block length
	}
}

func TestDensityCode(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		response []byte
		want     byte
		wantOK   bool
	}{
		// The bytes an LTO-5 cartridge in an mhvtl LTO-6 drive actually returns.
		"real LTO-5 response": {
			response: []byte{0x00, 0xea, 0x58, 0x10, 0x00, 0x00, 0x00, 0x08, 0x58, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			want:     0x58,
			wantOK:   true,
		},
		"LTO-9 density":    {response: modeSenseResponse(0x60), want: 0x60, wantOK: true},
		"truncated header": {response: []byte{0x00, 0xea, 0x58}, wantOK: false},
		// No descriptor must not be read as density 0x00.
		"no block descriptor": {
			response: []byte{0x00, 0x06, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00},
			wantOK:   false,
		},
		"descriptor length exceeds response": {
			response: []byte{0x00, 0xea, 0x58, 0x10, 0x00, 0x00, 0x00, 0x40, 0x58},
			wantOK:   false,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			code, ok := densityCode(test.response)
			assert.Equal(t, test.wantOK, ok)

			if test.wantOK {
				assert.Equal(t, test.want, code)
			}
		})
	}
}

// TestLTODensityCodes pins the table to mhvtl's include/vtlcart.h values.
func TestLTODensityCodes(t *testing.T) {
	t.Parallel()

	tests := map[byte]string{
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

	assert.Equal(t, tests, ltoDensityCodes)

	_, known := ltoDensityCodes[0x59]
	assert.False(t, known, "an unassigned density code must not map to a generation")
}
