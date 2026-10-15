// Package tape drives the tape library changer and drives. Device paths are
// injected so the same code targets mhvtl and real LTO hardware.
package tape

// Barcode is the canonical tape identity: the changer's volume tag (SPEC.md §6).
type Barcode string

// DriveElement is a tape drive reported by READ ELEMENT STATUS.
type DriveElement struct {
	// Address is the 0-based friendly drive index.
	Address int
	Barcode Barcode
	Loaded  bool
	// SourceSlot is the storage slot the tape came from; 0 when empty or unknown.
	SourceSlot int
	// Serial is the unit serial from the DVCID identifier (matches INQUIRY VPD
	// 0x80); Load pairs device nodes to elements by it (#137). Empty when the
	// library does not report DVCID.
	Serial string
}

// StorageElement is a storage slot reported by READ ELEMENT STATUS.
type StorageElement struct {
	// Address is the 1-based friendly slot number.
	Address int
	Barcode Barcode
	Full    bool
}

// IOElement is an import/export station slot reported by READ ELEMENT STATUS.
type IOElement struct {
	// Address is the friendly slot number, numbered after the storage slots.
	Address int
	Barcode Barcode
	Full    bool
	// Accessible is the ACCESS bit: false while the operator has the station
	// open. The Eject phase uses it to auto-resume (SPEC §4.3 phase 8).
	Accessible bool
}

// Inventory is the decoded result of a READ ELEMENT STATUS query.
type Inventory struct {
	Drives  []DriveElement
	Slots   []StorageElement
	IOSlots []IOElement
	// IOAccessReported is true when the library reports I/O elements, and so
	// their ACCESS bits.
	IOAccessReported bool
}

// TapeAlertFlag is a single TapeAlert indicator from log page 0x2e.
type TapeAlertFlag struct {
	Number      int
	Description string
	Set         bool
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
	// Repositions counts write suspensions (back-hitches) from log page 0x30
	// parameter 5. Meaningful only when RepositionsMeasured is true.
	Repositions int64
	// RepositionsMeasured separates "measured zero" from "not measured", so an
	// unread counter can never certify a clean streaming write.
	RepositionsMeasured bool
}
