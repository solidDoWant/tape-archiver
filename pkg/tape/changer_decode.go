package tape

import (
	"bytes"
	"fmt"
	"strings"
)

// Platform-independent halves of the SG_IO changer (element-address arithmetic
// and the 0x1D / 0xB8 response decoders), kept free of syscalls so they are
// unit-testable everywhere.

// SCSI element type codes (SMC-3).
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
	modePageElementAddr = 0x1D
	// modePageCodeMask strips PS (bit 7) and SPF (bit 6).
	modePageCodeMask = 0x3F
)

const (
	pvoltagBit = 0x80
	svalidBit  = 0x80
	// The first 32 bytes of the 36-byte primary volume tag are the barcode.
	primaryVolumeTagOffset = 12
	primaryVolumeTagLen    = 32
	// primaryVolumeTagFieldLen is the full tag width; the DVCID identifier of a
	// data-transfer descriptor follows it when PVOLTAG is set.
	primaryVolumeTagFieldLen = 36
)

// DVCID designation descriptor layout (SPC) and the code sets we read a serial
// from.
const (
	deviceIDHeaderLen     = 4
	deviceIDCodeSetOffset = 0
	deviceIDLengthOffset  = 3
	// deviceIDCodeSetMask drops the protocol identifier in the upper nibble.
	deviceIDCodeSetMask = 0x0F
	codeSetASCII        = 0x02
	codeSetUTF8         = 0x03
)

// MODE SENSE(6) response layout.
const (
	modeParamHeaderLen          = 4
	modeParamBlockDescLenOffset = 3
)

// Field offsets within the Element Address Assignment page body.
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

// READ ELEMENT STATUS response layout.
const (
	elementStatusHeaderLen = 8
	// elementStatusReportOffset is the 24-bit "byte count of report available",
	// which exceeds the bytes transferred only when the report was truncated.
	elementStatusReportOffset  = 5
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

// elementAddressing maps raw SCSI element addresses to friendly drive/slot
// numbers using the same computation as mtx, so blankSlots configs stay
// compatible.
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

func (a *elementAddressing) friendlyDrive(raw int) int { return raw - a.firstDTE }

func (a *elementAddressing) friendlyStorage(raw int) int { return raw - a.firstStorage + 1 }

func (a *elementAddressing) friendlyIO(raw int) int { return a.numStorage + (raw - a.firstIE) + 1 }

func (a *elementAddressing) isStorageRaw(raw int) bool {
	return raw >= a.firstStorage && raw < a.firstStorage+a.numStorage
}

func (a *elementAddressing) rawStorage(slot int) (int, error) {
	if slot < 1 || slot > a.numStorage {
		return 0, fmt.Errorf("storage slot %d out of range (1..%d)", slot, a.numStorage)
	}

	return a.firstStorage + (slot - 1), nil
}

func (a *elementAddressing) rawDrive(index int) (int, error) {
	if index < 0 || index >= a.numDTE {
		return 0, fmt.Errorf("drive index %d out of range (0..%d)", index, a.numDTE-1)
	}

	return a.firstDTE + index, nil
}

// rawSlot converts a slot in mtx's combined storage+I/O numbering to its raw
// address.
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

// transportElement returns the robot address for MOVE MEDIUM; 0 selects the
// default transport when the library declares none.
func (a *elementAddressing) transportElement() int {
	if a.numMTE > 0 {
		return a.firstMTE
	}

	return 0
}

// parseElementAddressing decodes a MODE SENSE(6) response for the Element
// Address Assignment page (0x1D).
func parseElementAddressing(data []byte) (*elementAddressing, error) {
	if len(data) < modeParamHeaderLen {
		return nil, fmt.Errorf("mode sense: short response (%d bytes)", len(data))
	}

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
// Inventory.
func decodeElementStatus(data []byte, addressing *elementAddressing) (Inventory, error) {
	var inv Inventory

	if len(data) < elementStatusHeaderLen {
		return inv, fmt.Errorf("read element status: short response (%d bytes)", len(data))
	}

	// Devices may silently truncate the report to the transfer buffer; refuse a
	// partial inventory rather than plan moves from wrong state.
	if reportByteCount := be24(data, elementStatusReportOffset); reportByteCount > len(data)-elementStatusHeaderLen {
		return Inventory{}, fmt.Errorf(
			"read element status: truncated report (%d report bytes claimed, %d transferred)",
			reportByteCount, len(data)-elementStatusHeaderLen)
	}

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

// decodeDescriptor decodes one element descriptor and appends it to inv.
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

		if full && len(desc) >= descSourceMinLen && desc[descSVALIDOffset]&svalidBit != 0 {
			if src := int(be16(desc, descSourceOffset)); addressing.isStorageRaw(src) {
				el.SourceSlot = addressing.friendlyStorage(src)
			}
		}

		// The DVCID serial pairs a drive device node to this element (#137).
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
		// READ ELEMENT STATUS always carries ACCESS (mtx's text output drops it).
		inv.IOAccessReported = true

	case elementTypeMediumTransport:
		// The robot is not surfaced in Inventory.
	}
}

// decodeDriveSerial extracts a drive's unit serial from the DVCID identifier
// appended to its descriptor, returning "" when absent or not printable text.
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

// serialFromDeviceID returns the last token of a T10 device identifier (vendor,
// product, serial), which matches the serial in INQUIRY VPD page 0x80.
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

// trimVolumeTag extracts a barcode from a space-padded, possibly
// null-terminated volume tag field.
func trimVolumeTag(field []byte) Barcode {
	if i := bytes.IndexByte(field, 0); i >= 0 {
		field = field[:i]
	}

	return Barcode(strings.TrimSpace(string(field)))
}

func be16(b []byte, off int) uint16 {
	return uint16(b[off])<<8 | uint16(b[off+1])
}

func be24(b []byte, off int) int {
	return int(b[off])<<16 | int(b[off+1])<<8 | int(b[off+2])
}
