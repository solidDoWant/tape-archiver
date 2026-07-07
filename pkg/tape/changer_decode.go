package tape

import (
	"bytes"
	"fmt"
	"strings"
)

// This file holds the platform-independent halves of the SG_IO changer: the
// raw↔friendly element-address arithmetic and the binary decoders for the
// MODE SENSE Element Address Assignment page (0x1D) and READ ELEMENT STATUS
// (0xB8) responses. Keeping them free of os/syscall makes them unit-testable on
// every platform; the ioctl plumbing lives in changer_linux.go.

// SCSI element type codes (SMC-3), as reported in READ ELEMENT STATUS pages and
// used to select the mode-page fields for address translation.
const (
	elementTypeMediumTransport = 1 // robot arm / picker
	elementTypeStorage         = 2 // storage slot
	elementTypeImportExport    = 3 // import/export (I/O station) slot
	elementTypeDataTransfer    = 4 // tape drive
)

// Element descriptor flag bits (READ ELEMENT STATUS, descriptor byte 2).
const (
	elementFlagFull   = 0x01 // FULL: an element holds media
	elementFlagAccess = 0x08 // ACCESS: the robot can reach the element (I/O door closed)
)

const (
	// modePageElementAddr is the Element Address Assignment mode page.
	modePageElementAddr = 0x1D
	// modePageCodeMask masks the page code out of a mode page's first byte
	// (bit 7 is PS, bit 6 is SPF).
	modePageCodeMask = 0x3F
)

const (
	// pvoltagBit is the PVOLTAG flag in a READ ELEMENT STATUS page header (at
	// pageFlagsOffset): when set, each descriptor carries a primary volume tag.
	pvoltagBit = 0x80
	// svalidBit is the SVALID flag in a data-transfer element descriptor (at
	// descSVALIDOffset): when set, the source storage element address is valid.
	svalidBit = 0x80
	// primaryVolumeTagOffset and primaryVolumeTagLen bound the barcode within an
	// element descriptor when PVOLTAG is set. The full primary volume tag field
	// is 36 bytes; the first 32 are the volume identifier (barcode), the rest a
	// reserved area and sequence number we do not use.
	primaryVolumeTagOffset = 12
	primaryVolumeTagLen    = 32
	// primaryVolumeTagFieldLen is the full width of the primary volume tag field
	// (32-byte volume identifier + a 4-byte reserved/sequence area). When PVOLTAG
	// is set the field is present in every descriptor, so the DVCID device
	// identifier that follows a data-transfer descriptor starts this far past the
	// volume tag offset.
	primaryVolumeTagFieldLen = 36
)

// DVCID primary device identifier (a SPC designation descriptor appended to each
// data-transfer element descriptor when DVCID is set in the READ ELEMENT STATUS
// CDB) layout and the code sets we decode a serial from.
const (
	// deviceIDHeaderLen is the designation descriptor header: byte 0 code set,
	// byte 1 designator type, byte 2 reserved, byte 3 designator length.
	deviceIDHeaderLen = 4
	// deviceIDCodeSetOffset and deviceIDLengthOffset locate the code set and the
	// designator length within that header.
	deviceIDCodeSetOffset = 0
	deviceIDLengthOffset  = 3
	// deviceIDCodeSetMask masks the code set out of byte 0 (the upper nibble is
	// the protocol identifier).
	deviceIDCodeSetMask = 0x0F
	// codeSetASCII and codeSetUTF8 are the printable-text code sets we extract a
	// serial from; a binary (code set 1) designator carries no ASCII serial.
	codeSetASCII = 0x02
	codeSetUTF8  = 0x03
)

// MODE SENSE(6) response layout (Element Address Assignment page). The page
// follows a 4-byte mode parameter header plus any block descriptors.
const (
	// modeParamHeaderLen is the length of the MODE SENSE(6) mode parameter header.
	modeParamHeaderLen = 4
	// modeParamBlockDescLenOffset is the header byte holding the block descriptor
	// length (the page starts after the header and those descriptors).
	modeParamBlockDescLenOffset = 3
)

// Byte offsets of the (first-address, count) fields within the Element Address
// Assignment mode page body (after its 2-byte page code / length header), plus
// the body length that must be present to read them all.
const (
	eaaFirstMediumTransport = 2
	eaaNumMediumTransport   = 4
	eaaFirstStorage         = 6
	eaaNumStorage           = 8
	eaaFirstImportExport    = 10
	eaaNumImportExport      = 12
	eaaFirstDataTransfer    = 14
	eaaNumDataTransfer      = 16
	eaaBodyLen              = 18
)

// READ ELEMENT STATUS response layout: an element status data header, then one
// element status page per element type, each with its own header followed by
// fixed-length element descriptors.
const (
	// elementStatusHeaderLen is the length of the element status data header.
	elementStatusHeaderLen = 8
	// elementStatusReportOffset is the offset of the "byte count of report
	// available" field (24-bit) within the element status data header. It counts
	// the report bytes that follow the header, and equals the bytes transferred
	// after the header when — and only when — the response was not truncated.
	elementStatusReportOffset = 5
	// elementStatusPageHeaderLen is the length of each element status page header.
	elementStatusPageHeaderLen = 8
)

// Byte offsets within an element status page header.
const (
	pageFlagsOffset     = 1 // PVOLTAG flag byte
	pageDescLenOffset   = 2 // element descriptor length (16-bit)
	pageByteCountOffset = 5 // total descriptor byte count (24-bit)
)

// Byte offsets and minimum lengths within an element descriptor.
const (
	descAddressOffset = 0  // element address (16-bit)
	descFlagsOffset   = 2  // FULL (0x01) / ACCESS (0x08) flags
	descMinLen        = 3  // enough for the address + flags prefix
	descSVALIDOffset  = 9  // SVALID flag byte
	descSourceOffset  = 10 // source storage element address (16-bit)
	descSourceMinLen  = 12 // enough to read SVALID + source address
)

// elementAddressing maps between a library's raw SCSI element addresses and the
// friendly drive/slot numbers callers use, derived from the Element Address
// Assignment mode page (0x1D). This is the same computation mtx performs, so the
// friendly numbers — and therefore blankSlots configs — are byte-compatible.
//
// Friendly numbering:
//   - drive index d          ↔ raw firstDTE + d          (0-based)
//   - storage slot s         ↔ raw firstStorage + (s-1)  (1-based)
//   - I/O slot numStorage+j  ↔ raw firstIE + (j-1)       (j 1-based; I/O slots
//     are numbered immediately after the storage slots)
type elementAddressing struct {
	firstMTE, numMTE         int
	firstStorage, numStorage int
	firstIE, numIE           int
	firstDTE, numDTE         int
}

// friendlyDrive converts a raw data-transfer element address to a 0-based drive
// index.
func (a *elementAddressing) friendlyDrive(raw int) int { return raw - a.firstDTE }

// friendlyStorage converts a raw storage element address to a 1-based storage
// slot number.
func (a *elementAddressing) friendlyStorage(raw int) int { return raw - a.firstStorage + 1 }

// friendlyIO converts a raw import/export element address to its friendly slot
// number (numbered after the storage slots).
func (a *elementAddressing) friendlyIO(raw int) int { return a.numStorage + (raw - a.firstIE) + 1 }

// isStorageRaw reports whether a raw address falls within the storage range.
func (a *elementAddressing) isStorageRaw(raw int) bool {
	return raw >= a.firstStorage && raw < a.firstStorage+a.numStorage
}

// rawStorage converts a 1-based friendly storage slot to its raw address.
func (a *elementAddressing) rawStorage(slot int) (int, error) {
	if slot < 1 || slot > a.numStorage {
		return 0, fmt.Errorf("storage slot %d out of range (1..%d)", slot, a.numStorage)
	}

	return a.firstStorage + (slot - 1), nil
}

// rawDrive converts a 0-based friendly drive index to its raw address.
func (a *elementAddressing) rawDrive(index int) (int, error) {
	if index < 0 || index >= a.numDTE {
		return 0, fmt.Errorf("drive index %d out of range (0..%d)", index, a.numDTE-1)
	}

	return a.firstDTE + index, nil
}

// rawSlot converts a friendly slot number in mtx's combined storage+I/O
// numbering (1..numStorage are storage slots, numStorage+1..numStorage+numIE are
// I/O slots) to its raw element address. Transfer uses this for both endpoints,
// which may be either a storage slot or an I/O-station slot.
func (a *elementAddressing) rawSlot(friendly int) (int, error) {
	switch {
	case friendly >= 1 && friendly <= a.numStorage:
		return a.firstStorage + (friendly - 1), nil
	case friendly > a.numStorage && friendly <= a.numStorage+a.numIE:
		return a.firstIE + (friendly - a.numStorage - 1), nil
	default:
		return 0, fmt.Errorf("slot %d out of range (storage 1..%d, I/O %d..%d)",
			friendly, a.numStorage, a.numStorage+1, a.numStorage+a.numIE)
	}
}

// transportElement returns the raw medium-transport (robot) element address to
// use as MOVE MEDIUM's first operand. It is the first transport element the
// library declares; libraries with no transport element use 0 (the default
// transport).
func (a *elementAddressing) transportElement() int {
	if a.numMTE > 0 {
		return a.firstMTE
	}

	return 0
}

// parseElementAddressing decodes a MODE SENSE(6) response for the Element
// Address Assignment page (0x1D). The response is a 4-byte mode parameter header
// (whose byte 3 is the block descriptor length), optional block descriptors,
// then the mode page: byte 0 page code, byte 1 page length, then four
// (first-address, count) 16-bit big-endian pairs for medium-transport, storage,
// import/export, and data-transfer elements.
func parseElementAddressing(data []byte) (*elementAddressing, error) {
	if len(data) < modeParamHeaderLen {
		return nil, fmt.Errorf("mode sense: short response (%d bytes)", len(data))
	}

	// Skip the mode parameter header and any block descriptors it declares.
	page := modeParamHeaderLen + int(data[modeParamBlockDescLenOffset])

	if len(data) < page+eaaBodyLen {
		return nil, fmt.Errorf("mode sense: truncated page 0x1D (%d bytes, page at %d)", len(data), page)
	}

	if code := data[page] & modePageCodeMask; code != modePageElementAddr {
		return nil, fmt.Errorf("mode sense: unexpected page 0x%02x (want 0x%02x)", code, modePageElementAddr)
	}

	field := func(off int) int { return int(be16(data, page+off)) }

	return &elementAddressing{
		firstMTE: field(eaaFirstMediumTransport), numMTE: field(eaaNumMediumTransport),
		firstStorage: field(eaaFirstStorage), numStorage: field(eaaNumStorage),
		firstIE: field(eaaFirstImportExport), numIE: field(eaaNumImportExport),
		firstDTE: field(eaaFirstDataTransfer), numDTE: field(eaaNumDataTransfer),
	}, nil
}

// decodeElementStatus decodes a READ ELEMENT STATUS (0xB8) response into an
// Inventory, using addressing to translate raw element addresses to friendly
// numbers. The response is an 8-byte status header followed by one Element
// Status Page per element type; each page has an 8-byte header (type code, a
// PVOLTAG flag, the per-descriptor length, and the total descriptor byte count)
// followed by fixed-length element descriptors.
//
// The device may truncate its report to the transfer buffer without signalling
// an error, in which case the descriptor walk would stop early and yield a
// partial Inventory. To catch that, the header's 24-bit "byte count of report
// available" is compared against the report bytes actually transferred (after
// the header); a report claiming more than was transferred is rejected as
// truncated rather than decoded into a partial inventory.
func decodeElementStatus(data []byte, addressing *elementAddressing) (Inventory, error) {
	var inv Inventory

	if len(data) < elementStatusHeaderLen {
		return inv, fmt.Errorf("read element status: short response (%d bytes)", len(data))
	}

	// The report byte count covers the bytes after the 8-byte header. If it
	// exceeds what was transferred, the library truncated its report to the
	// transfer buffer and the pages below are incomplete — refuse to decode a
	// partial inventory rather than plan moves from wrong state.
	if reportByteCount := be24(data, elementStatusReportOffset); reportByteCount > len(data)-elementStatusHeaderLen {
		return Inventory{}, fmt.Errorf(
			"read element status: truncated report (%d report bytes claimed, %d transferred)",
			reportByteCount, len(data)-elementStatusHeaderLen)
	}

	// Walk the element status pages that follow the status data header.
	for pos := elementStatusHeaderLen; pos+elementStatusPageHeaderLen <= len(data); {
		elementType := data[pos]
		pvoltag := data[pos+pageFlagsOffset]&pvoltagBit != 0
		descLen := int(be16(data, pos+pageDescLenOffset))
		byteCount := be24(data, pos+pageByteCountOffset)
		descStart := pos + elementStatusPageHeaderLen

		if descLen <= 0 {
			break // malformed; avoid an infinite loop
		}

		limit := descStart + byteCount
		for d := descStart; d+descLen <= limit && d+descLen <= len(data); d += descLen {
			decodeDescriptor(elementType, pvoltag, data[d:d+descLen], addressing, &inv)
		}

		pos = descStart + byteCount
	}

	return inv, nil
}

// decodeDescriptor decodes one element descriptor and appends it to inv. The
// common descriptor prefix is: bytes 0-1 element address, byte 2 flags (FULL
// 0x01, ACCESS 0x08). A data-transfer descriptor additionally carries an SVALID
// flag (byte 9, 0x80) and source storage element address (bytes 10-11). When the
// page's PVOLTAG flag is set and the element is full, the barcode is the primary
// volume tag at byte 12.
func decodeDescriptor(elementType byte, pvoltag bool, desc []byte, addressing *elementAddressing, inv *Inventory) {
	if len(desc) < descMinLen {
		return
	}

	addr := int(be16(desc, descAddressOffset))
	full := desc[descFlagsOffset]&elementFlagFull != 0
	access := desc[descFlagsOffset]&elementFlagAccess != 0

	var barcode Barcode
	if full && pvoltag && len(desc) >= primaryVolumeTagOffset+primaryVolumeTagLen {
		barcode = trimVolumeTag(desc[primaryVolumeTagOffset : primaryVolumeTagOffset+primaryVolumeTagLen])
	}

	switch elementType {
	case elementTypeDataTransfer:
		el := DriveElement{Address: addressing.friendlyDrive(addr), Loaded: full, Barcode: barcode}

		// Recover the source storage slot for a loaded drive, mirroring mtx's
		// "(Storage Element N Loaded)" annotation, when the library reports it.
		if full && len(desc) >= descSourceMinLen && desc[descSVALIDOffset]&svalidBit != 0 {
			if src := int(be16(desc, descSourceOffset)); addressing.isStorageRaw(src) {
				el.SourceSlot = addressing.friendlyStorage(src)
			}
		}

		// Pair a drive device node to this element by its unit serial: decode the
		// DVCID device identifier that follows the (optional) primary volume tag
		// (issue #137). Absent when the library does not implement DVCID.
		el.Serial = decodeDriveSerial(desc, pvoltag)

		inv.Drives = append(inv.Drives, el)

	case elementTypeStorage:
		inv.Slots = append(inv.Slots, StorageElement{
			Address: addressing.friendlyStorage(addr),
			Full:    full,
			Barcode: barcode,
		})

	case elementTypeImportExport:
		inv.IOSlots = append(inv.IOSlots, IOElement{
			Address:    addressing.friendlyIO(addr),
			Full:       full,
			Barcode:    barcode,
			Accessible: access,
		})
		// READ ELEMENT STATUS always carries the ACCESS bit, so any library that
		// answers with I/O elements reports access state — unlike the mtx text
		// scrape this replaces, which dropped it entirely.
		inv.IOAccessReported = true

	case elementTypeMediumTransport:
		// The robot itself is not surfaced in Inventory; its address is taken
		// from the mode page for MOVE MEDIUM.
	}
}

// decodeDriveSerial extracts a data-transfer element's unit serial from the DVCID
// device identifier appended to its descriptor. The identifier is a single SPC
// designation descriptor sitting after the 12-byte descriptor prefix and, when the
// page's PVOLTAG flag is set, the 36-byte primary volume tag. It returns "" when
// the descriptor carries no identifier (the library did not report DVCID, or the
// descriptor is too short) or the designator is not printable text — so a
// non-reporting library still decodes with an empty Serial rather than failing.
func decodeDriveSerial(desc []byte, pvoltag bool) string {
	start := primaryVolumeTagOffset
	if pvoltag {
		start += primaryVolumeTagFieldLen
	}

	if len(desc) < start+deviceIDHeaderLen {
		return ""
	}

	header := desc[start:]

	codeSet := header[deviceIDCodeSetOffset] & deviceIDCodeSetMask
	if codeSet != codeSetASCII && codeSet != codeSetUTF8 {
		return ""
	}

	idLen := int(header[deviceIDLengthOffset])
	if idLen <= 0 || deviceIDHeaderLen+idLen > len(header) {
		return ""
	}

	return serialFromDeviceID(header[deviceIDHeaderLen : deviceIDHeaderLen+idLen])
}

// serialFromDeviceID reduces a T10 device-identifier string to the drive's unit
// serial: the last whitespace-delimited token. A T10 vendor designator is the
// vendor id, optionally the product id, then the serial (all space-padded); a
// serial contains no spaces, so the final token is the serial — matching the value
// the drive returns in INQUIRY VPD page 0x80. It handles a null-terminated field
// and returns "" when no token is present.
func serialFromDeviceID(identifier []byte) string {
	if i := bytes.IndexByte(identifier, 0); i >= 0 {
		identifier = identifier[:i]
	}

	fields := strings.Fields(string(identifier))
	if len(fields) == 0 {
		return ""
	}

	return fields[len(fields)-1]
}

// trimVolumeTag extracts a barcode from a fixed-width primary volume tag field.
// SCSI volume tags are left-justified and space-padded (and may be
// null-terminated); the barcode is the field up to the first null with
// surrounding whitespace removed.
func trimVolumeTag(field []byte) Barcode {
	if i := bytes.IndexByte(field, 0); i >= 0 {
		field = field[:i]
	}

	return Barcode(strings.TrimSpace(string(field)))
}

// be16 reads a 16-bit big-endian value at off. Callers guarantee the bounds.
func be16(b []byte, off int) uint16 {
	return uint16(b[off])<<8 | uint16(b[off+1])
}

// be24 reads a 24-bit big-endian value at off. Callers guarantee the bounds.
func be24(b []byte, off int) int {
	return int(b[off])<<16 | int(b[off+1])<<8 | int(b[off+2])
}
