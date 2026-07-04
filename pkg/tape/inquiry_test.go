package tape

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// standardInquiry builds a standard INQUIRY (0x12) response with the given ASCII
// identity strings at their SPC-defined offsets, space-padded to their field
// widths, so the decoder is exercised against a byte layout matching a real drive.
func standardInquiry(vendor, product, revision string) []byte {
	data := make([]byte, 36)
	data[0] = 0x01 // peripheral device type: sequential-access (tape)
	data[4] = 31   // additional length

	copyPadded := func(offset, length int, value string) {
		for i := 0; i < length; i++ {
			if i < len(value) {
				data[offset+i] = value[i]
			} else {
				data[offset+i] = ' '
			}
		}
	}

	copyPadded(inquiryVendorOffset, inquiryVendorLen, vendor)
	copyPadded(inquiryProductOffset, inquiryProductLen, product)
	copyPadded(inquiryRevisionOffset, inquiryRevisionLen, revision)

	return data
}

// unitSerialPage builds a Unit Serial Number VPD page (0x80) response carrying
// the given serial.
func unitSerialPage(serial string) []byte {
	data := []byte{0x01, vpdUnitSerial, 0x00, byte(len(serial))}

	return append(data, []byte(serial)...)
}

// TestDecodeStandardInquiry checks vendor/product/revision extraction from a
// well-formed response and from a truncated one.
func TestDecodeStandardInquiry(t *testing.T) {
	t.Parallel()

	vendor, product, revision := decodeStandardInquiry(standardInquiry("IBM", "ULT3580-TD6", "1760"))
	assert.Equal(t, "IBM", vendor)
	assert.Equal(t, "ULT3580-TD6", product)
	assert.Equal(t, "1760", revision)

	// A response truncated before the revision field yields an empty revision but
	// still returns the fields it reaches.
	shortVendor, shortProduct, shortRevision := decodeStandardInquiry(standardInquiry("STK", "L700", "")[:20])
	assert.Equal(t, "STK", shortVendor)
	assert.Equal(t, "L700", shortProduct)
	assert.Empty(t, shortRevision)

	// A response too short to reach any identity field yields all empty.
	emptyVendor, emptyProduct, emptyRevision := decodeStandardInquiry([]byte{0x01, 0, 0, 0})
	assert.Empty(t, emptyVendor)
	assert.Empty(t, emptyProduct)
	assert.Empty(t, emptyRevision)
}

// TestDecodeUnitSerial checks serial extraction, including a page whose declared
// length exceeds the buffer and a header-only page.
func TestDecodeUnitSerial(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "1068000073", decodeUnitSerial(unitSerialPage("1068000073")))

	// Trailing pad is trimmed.
	assert.Equal(t, "XYZZY_A1", decodeUnitSerial(unitSerialPage("XYZZY_A1  ")))

	// A declared page length past the buffer is clamped to what is present.
	overlong := []byte{0x01, vpdUnitSerial, 0x00, 0x20, 'A', 'B', 'C'}
	assert.Equal(t, "ABC", decodeUnitSerial(overlong))

	// A header-only page (no serial bytes) yields empty.
	assert.Empty(t, decodeUnitSerial([]byte{0x01, vpdUnitSerial, 0x00, 0x00}))
}

// TestDeviceInfoModel checks the vendor+product join, including the empty cases.
func TestDeviceInfoModel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "IBM ULT3580-TD6", DeviceInfo{Vendor: "IBM", Product: "ULT3580-TD6"}.Model())
	// A device that reports no vendor (some changers) still yields the product alone.
	assert.Equal(t, "L700", DeviceInfo{Product: "L700"}.Model())
	assert.Empty(t, DeviceInfo{}.Model())
}

// TestLTOGenerationFromProduct covers the product id → generation mapping across
// IBM full/half-height, HPE, and unmapped forms.
func TestLTOGenerationFromProduct(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		product string
		want    string
	}{
		"IBM full height LTO-6": {"ULT3580-TD6", "LTO-6"},
		"IBM full height LTO-8": {"ULT3580-TD8", "LTO-8"},
		"IBM half height LTO-8": {"ULT3580-HH8", "LTO-8"},
		"IBM half height LTO-9": {"ULT3580-HH9", "LTO-9"},
		"IBM legacy Ultrium":    {"ULTRIUM-TD5", "LTO-5"},
		"HPE LTFS-era":          {"Ultrium 7-SCSI", "LTO-7"},
		"changer L700":          {"L700", "unknown"},
		"pre-LTFS HP model":     {"Ultrium 960", "unknown"},
		"pre-LTFS IBM LTO-4":    {"ULT3580-TD4", "unknown"},
		"empty":                 {"", "unknown"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, DeviceInfo{Product: test.product}.LTOGeneration())
		})
	}
}
