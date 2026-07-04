package tape

import "strings"

// This file holds the platform-independent half of SCSI INQUIRY support: the
// DeviceInfo value the report records as drive/library provenance, the product
// id → LTO generation mapping, and the binary decoders for the standard INQUIRY
// (0x12) response and the Unit Serial Number VPD page (0x80). Keeping them free
// of os/syscall makes them unit-testable on every platform; the SG_IO plumbing
// and the Drive/Changer.Inquire entry points live in inquiry_linux.go.

// unknownGeneration is returned by LTOGeneration when the product id matches no
// recognized LTO drive, so the report renders a visible placeholder rather than
// a blank field.
const unknownGeneration = "unknown"

// Standard INQUIRY (0x12, EVPD=0) response layout: the ASCII identity strings at
// their fixed offsets (SPC "Standard INQUIRY data"). Each field is left-justified
// and space-padded.
const (
	inquiryVendorOffset   = 8 // vendor identification (T10 vendor id)
	inquiryVendorLen      = 8
	inquiryProductOffset  = 16 // product identification
	inquiryProductLen     = 16
	inquiryRevisionOffset = 32 // product revision level
	inquiryRevisionLen    = 4
)

// Unit Serial Number VPD page (0x80) response layout: a 4-byte header (byte 3 is
// the page length) followed by the ASCII serial number.
const (
	vpdPageLengthOffset = 3 // page length (8-bit) in the VPD page header
	vpdSerialOffset     = 4 // product serial number follows the header
)

// DeviceInfo is the SCSI identity of a tape drive or media changer, read from a
// standard INQUIRY (vendor/product/revision) plus the Unit Serial Number VPD
// page. It is recorded in the run report's build parameters as provenance for the
// hardware that wrote the tapes (SPEC §9).
type DeviceInfo struct {
	// Vendor is the T10 vendor identification, e.g. "IBM" or "STK".
	Vendor string
	// Product is the product identification, e.g. "ULT3580-TD6" or "L700".
	Product string
	// Revision is the product revision level (firmware revision).
	Revision string
	// Serial is the unit serial number from VPD page 0x80. It is empty when the
	// device does not implement the page.
	Serial string
}

// Model returns the human-readable drive/library model — vendor and product
// joined, e.g. "IBM ULT3580-TD6". It is empty only when both are empty.
func (i DeviceInfo) Model() string {
	return strings.TrimSpace(i.Vendor + " " + i.Product)
}

// LTOGeneration returns the LTO generation required to read a tape written by
// this drive (e.g. "LTO-6"), derived from the INQUIRY product id — the fact a
// future recoverer actually needs (SPEC §9, docs/report.md). It returns
// unknownGeneration when the product id matches no recognized LTO drive (e.g. a
// media changer, which has no generation).
func (i DeviceInfo) LTOGeneration() string {
	return ltoGenerationFromProduct(i.Product)
}

// ltoGenerationFromProduct maps a SCSI product id to its LTO generation. It
// recognizes IBM Ultrium drives ("ULT3580-TD6" full height / "ULT3580-HH8" half
// height / "ULTRIUM-TD5"), whose trailing character is the generation digit, and
// HPE LTFS-era Ultrium drives ("ULTRIUM 6-SCSI"), whose generation digit
// immediately precedes a hyphen. Only LTO-5..9 — the LTFS-capable generations
// this project supports (docs/report.md) — are mapped; anything else, including
// pre-LTFS HP model-number names like "ULTRIUM 960", yields unknownGeneration.
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

// decodeStandardInquiry extracts the vendor, product, and revision identity
// strings from a standard INQUIRY (0x12) response, tolerating a short buffer by
// returning empty strings for fields the response does not reach.
func decodeStandardInquiry(data []byte) (vendor, product, revision string) {
	vendor = asciiField(data, inquiryVendorOffset, inquiryVendorLen)
	product = asciiField(data, inquiryProductOffset, inquiryProductLen)
	revision = asciiField(data, inquiryRevisionOffset, inquiryRevisionLen)

	return vendor, product, revision
}

// decodeUnitSerial extracts the serial number from a Unit Serial Number VPD page
// (0x80) response, bounded by the page-length header and the actual buffer. A
// short or empty page yields an empty string.
func decodeUnitSerial(data []byte) string {
	if len(data) <= vpdSerialOffset {
		return ""
	}

	end := min(vpdSerialOffset+int(data[vpdPageLengthOffset]), len(data))

	return strings.TrimSpace(string(data[vpdSerialOffset:end]))
}

// asciiField returns the space-trimmed ASCII string at [offset, offset+length)
// within data, clamped to the buffer so a truncated response yields whatever
// prefix is present (or "" when the field starts past the end).
func asciiField(data []byte, offset, length int) string {
	if offset >= len(data) {
		return ""
	}

	end := min(offset+length, len(data))

	return strings.TrimSpace(string(data[offset:end]))
}
