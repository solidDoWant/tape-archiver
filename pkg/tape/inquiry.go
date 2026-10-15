package tape

import "strings"

// unknownGeneration gives the report a visible placeholder, not a blank field.
const unknownGeneration = "unknown"

// Standard INQUIRY response layout (space-padded ASCII fields).
const (
	inquiryVendorOffset   = 8 // vendor identification (T10 vendor id)
	inquiryVendorLen      = 8
	inquiryProductOffset  = 16 // product identification
	inquiryProductLen     = 16
	inquiryRevisionOffset = 32 // product revision level
	inquiryRevisionLen    = 4
)

// Unit Serial Number VPD page (0x80) response layout.
const (
	vpdPageLengthOffset = 3 // page length (8-bit) in the VPD page header
	vpdSerialOffset     = 4 // product serial number follows the header
)

// DeviceInfo is the SCSI INQUIRY identity of a drive or changer, recorded in
// the report as provenance (SPEC §9).
type DeviceInfo struct {
	Vendor   string
	Product  string
	Revision string
	// Serial is empty when the device lacks VPD page 0x80.
	Serial string
}

// Model returns vendor and product joined, e.g. "IBM ULT3580-TD6".
func (i DeviceInfo) Model() string {
	return strings.TrimSpace(i.Vendor + " " + i.Product)
}

// LTOGeneration returns the drive's LTO generation (e.g. "LTO-6") from its
// product id, or unknownGeneration.
func (i DeviceInfo) LTOGeneration() string {
	return ltoGenerationFromProduct(i.Product)
}

// ltoGenerationFromProduct recognizes IBM ("ULT3580-TD6", "ULTRIUM-TD5") and
// HPE ("ULTRIUM 6-SCSI") product ids for the LTFS-capable LTO-5..9 only;
// pre-LTFS names like "ULTRIUM 960" are unknown.
func ltoGenerationFromProduct(product string) string {
	upper := strings.ToUpper(strings.TrimSpace(product))

	var generation byte

	switch {
	case strings.Contains(upper, "ULT3580-"), strings.Contains(upper, "ULTRIUM-"):
		generation = upper[len(upper)-1]
	case strings.HasPrefix(upper, "ULTRIUM "):
		if rest := strings.TrimPrefix(upper, "ULTRIUM "); len(rest) >= 2 && rest[1] == '-' {
			generation = rest[0]
		}
	}

	if generation >= '5' && generation <= '9' {
		return "LTO-" + string(generation)
	}

	return unknownGeneration
}

// decodeStandardInquiry tolerates a short buffer.
func decodeStandardInquiry(data []byte) (vendor, product, revision string) {
	vendor = asciiField(data, inquiryVendorOffset, inquiryVendorLen)
	product = asciiField(data, inquiryProductOffset, inquiryProductLen)
	revision = asciiField(data, inquiryRevisionOffset, inquiryRevisionLen)

	return vendor, product, revision
}

func decodeUnitSerial(data []byte) string {
	if len(data) <= vpdSerialOffset {
		return ""
	}

	end := min(vpdSerialOffset+int(data[vpdPageLengthOffset]), len(data))

	return strings.TrimSpace(string(data[vpdSerialOffset:end]))
}

// asciiField returns the trimmed field, clamped to the buffer.
func asciiField(data []byte, offset, length int) string {
	if offset >= len(data) {
		return ""
	}

	end := min(offset+length, len(data))

	return strings.TrimSpace(string(data[offset:end]))
}
