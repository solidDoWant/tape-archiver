// Package tape drives the tape library changer (directly via SG_IO) and drive
// (mt, sg_logs) tools. All device paths are injected so the same code targets
// both mhvtl virtual hardware and real LTO drives.
package tape

// Barcode is the canonical tape identity — the volume tag read by the library
// changer (SPEC.md §6).
type Barcode string

// DriveElement is a data-transfer element (tape drive) reported by READ ELEMENT
// STATUS.
type DriveElement struct {
	// Address is the friendly drive index (0-based), derived from the element
	// address assignment mode page.
	Address int
	// Barcode is the volume tag of the loaded tape, or empty if the drive is empty.
	Barcode Barcode
	// Loaded is true when a tape is in the drive.
	Loaded bool
	// SourceSlot is the storage slot number the tape was loaded from, when Loaded is true.
	// It is 0 when the drive is empty or the source slot is unknown.
	SourceSlot int
}

// StorageElement is a storage slot reported by READ ELEMENT STATUS.
type StorageElement struct {
	// Address is the friendly, 1-indexed storage slot number, derived from the
	// element address assignment mode page.
	Address int
	// Barcode is the volume tag of the stored tape, or empty if the slot is empty.
	Barcode Barcode
	// Full is true when a tape is in the slot.
	Full bool
}

// IOElement is an import/export (I/O) station slot reported by READ ELEMENT
// STATUS.
type IOElement struct {
	// Address is the friendly slot number (numbered after the storage slots),
	// derived from the element address assignment mode page.
	Address int
	// Barcode is the volume tag of the tape in the slot, or empty if empty.
	Barcode Barcode
	// Full is true when a tape is in the slot.
	Full bool
	// Accessible reflects the SCSI READ ELEMENT STATUS import/export ACCESS bit
	// for this slot: true when the changer robot can reach the element (the I/O
	// station door is closed), false when the operator has the station open. The
	// operator-in-the-loop Eject phase uses it to auto-resume once the operator has
	// cleared and closed the station (SPEC §4.3 phase 8).
	Accessible bool
}

// Inventory is the decoded result of a READ ELEMENT STATUS query.
type Inventory struct {
	Drives  []DriveElement
	Slots   []StorageElement
	IOSlots []IOElement
	// IOAccessReported is true when the library reports import/export elements
	// (see IOElement.Accessible). READ ELEMENT STATUS always carries the ACCESS
	// bit, so any library with an I/O station reports it — enabling the Eject
	// phase to detect the station door cycle and auto-resume (SPEC §4.3 phase 8).
	IOAccessReported bool
}

// TapeAlertFlag is a single TapeAlert indicator from log page 0x2e.
type TapeAlertFlag struct {
	// Number is the flag number (e.g. 1 for "Read warning").
	Number int
	// Description is the human-readable flag label from sg_logs output.
	Description string
	// Set is true when the flag is active (value 0x1).
	Set bool
}

// TapeAlertResult holds all TapeAlert flags parsed from sg_logs page 0x2e.
type TapeAlertResult struct {
	Flags []TapeAlertFlag
}

// AnySet returns true if any TapeAlert flag is active.
func (r TapeAlertResult) AnySet() bool {
	for _, f := range r.Flags {
		if f.Set {
			return true
		}
	}

	return false
}

// LogPageResult holds drive health indicators scraped from sg_logs.
type LogPageResult struct {
	TapeAlert TapeAlertResult
	// Repositions is the number of back-hitches (tape repositions) since the
	// last reset, from sequential-access log page 0x24. Zero when unavailable.
	Repositions int64
}
