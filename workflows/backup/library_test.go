package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// inventoryWith returns a minimal Inventory with the given drives and slots for
// use in table-driven tests of the pure library helpers.
func inventoryWith(drives []tape.DriveElement, slots []tape.StorageElement, ioSlots []tape.IOElement) tape.Inventory {
	return tape.Inventory{Drives: drives, Slots: slots, IOSlots: ioSlots}
}

func TestFindFreeStorageSlot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		slots      []tape.StorageElement
		preferSlot int
		want       int
	}{
		{
			name: "prefers prefer slot when empty",
			slots: []tape.StorageElement{
				{Address: 1, Full: true},
				{Address: 2, Full: false},
				{Address: 3, Full: false},
			},
			preferSlot: 2,
			want:       2,
		},
		{
			name: "falls back to first empty when prefer slot is full",
			slots: []tape.StorageElement{
				{Address: 1, Full: false},
				{Address: 2, Full: true},
				{Address: 3, Full: false},
			},
			preferSlot: 2,
			want:       1,
		},
		{
			name: "returns -1 when all slots are full",
			slots: []tape.StorageElement{
				{Address: 1, Full: true},
				{Address: 2, Full: true},
			},
			preferSlot: 1,
			want:       -1,
		},
		{
			name:       "returns -1 when inventory has no slots",
			slots:      nil,
			preferSlot: 0,
			want:       -1,
		},
		{
			name: "prefer slot not in list falls back to any empty",
			slots: []tape.StorageElement{
				{Address: 1, Full: false},
				{Address: 2, Full: true},
			},
			preferSlot: 99,
			want:       1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			inv := inventoryWith(nil, test.slots, nil)
			got := findFreeStorageSlot(inv, test.preferSlot)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestFindFreeIOSlot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ioSlots []tape.IOElement
		want    int
	}{
		{
			name: "returns first empty I/O slot",
			ioSlots: []tape.IOElement{
				{Address: 10, Full: true},
				{Address: 11, Full: false},
				{Address: 12, Full: false},
			},
			want: 11,
		},
		{
			name: "returns -1 when all I/O slots are full",
			ioSlots: []tape.IOElement{
				{Address: 10, Full: true},
				{Address: 11, Full: true},
			},
			want: -1,
		},
		{
			name:    "returns -1 when no I/O slots",
			ioSlots: nil,
			want:    -1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			inv := inventoryWith(nil, nil, test.ioSlots)
			got := findFreeIOSlot(inv)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestSlotBarcode(t *testing.T) {
	t.Parallel()

	slots := []tape.StorageElement{
		{Address: 1, Barcode: "AAA001L8", Full: true},
		{Address: 2, Barcode: "", Full: false},
		{Address: 5, Barcode: "BBB002L8", Full: true},
	}

	tests := []struct {
		name      string
		addr      int
		wantBC    tape.Barcode
		wantFound bool
	}{
		{name: "full slot returns barcode and true", addr: 1, wantBC: "AAA001L8", wantFound: true},
		{name: "empty slot returns empty barcode and false", addr: 2, wantBC: "", wantFound: false},
		{name: "non-contiguous address found correctly", addr: 5, wantBC: "BBB002L8", wantFound: true},
		{name: "absent address returns empty and false", addr: 99, wantBC: "", wantFound: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			inv := inventoryWith(nil, slots, nil)
			bc, found := slotBarcode(inv, test.addr)
			assert.Equal(t, test.wantBC, bc)
			assert.Equal(t, test.wantFound, found)
		})
	}
}

func TestReconcileLoad(t *testing.T) {
	t.Parallel()

	// Base inventory: drive 0 empty, slot 1 has "AAA001L8", slot 2 has "BBB002L8".
	baseSlots := []tape.StorageElement{
		{Address: 1, Barcode: "AAA001L8", Full: true},
		{Address: 2, Barcode: "BBB002L8", Full: true},
		{Address: 3, Full: false},
	}

	tests := []struct {
		name       string
		drives     []tape.DriveElement
		driveIndex int
		driveAddr  int
		targetSlot int
		// wantBarcode is the barcode that should end up in the drive; empty means
		// the call is expected to fail.
		wantBarcode tape.Barcode
		wantErr     require.ErrorAssertionFunc
	}{
		{
			name: "drive empty loads from target slot",
			drives: []tape.DriveElement{
				{Address: 0, Loaded: false},
			},
			driveIndex:  0,
			driveAddr:   0,
			targetSlot:  1,
			wantBarcode: "AAA001L8",
			wantErr:     require.Error, // no real changer; expect error from changer.Load
		},
		{
			name: "drive already loaded from target slot is idempotent",
			drives: []tape.DriveElement{
				{Address: 0, Loaded: true, Barcode: "AAA001L8", SourceSlot: 1},
			},
			driveIndex:  0,
			driveAddr:   0,
			targetSlot:  1,
			wantBarcode: "AAA001L8",
			wantErr:     require.NoError,
		},
		{
			name: "drive index out of range returns error",
			drives: []tape.DriveElement{
				{Address: 0, Loaded: false},
			},
			driveIndex: 5,
			driveAddr:  0,
			targetSlot: 1,
			wantErr:    require.Error,
		},
		{
			name: "target slot empty returns error",
			drives: []tape.DriveElement{
				{Address: 0, Loaded: false},
			},
			driveIndex: 0,
			driveAddr:  0,
			targetSlot: 3, // slot 3 is empty in baseSlots
			wantErr:    require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			inv := inventoryWith(test.drives, baseSlots, nil)
			// Use a nil changer device — the idempotent path returns before any
			// changer command; the other paths hit a command error which is the
			// expected test outcome for non-idempotent cases.
			changer := tape.NewChanger("")
			bc, err := reconcileLoad(t.Context(), changer, inv, test.driveIndex, test.driveAddr, test.targetSlot)
			test.wantErr(t, err)

			if err == nil {
				assert.Equal(t, test.wantBarcode, bc)
			}
		})
	}
}
