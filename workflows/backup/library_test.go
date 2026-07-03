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

// tapesFor builds a plan with tapeCount logical tapes and the given copy count.
// The archives are irrelevant to planDriveSets — it partitions by (tape, copy)
// pairs — so each tape carries a single placeholder archive.
func tapesFor(tapeCount, copies int) TapePlan {
	tapes := make([]PlannedTape, tapeCount)
	for i := range tapes {
		tapes[i] = PlannedTape{Archives: []PlannedArchive{{SourceIndex: 0}}}
	}

	return TapePlan{Copies: copies, Tapes: tapes}
}

func TestPlanDriveSets(t *testing.T) {
	t.Parallel()

	// blankSlots is a generous, distinct pool so every physical tape can claim its
	// own blank slot; the tests assert each slot is used at most once.
	blankSlots := []int{10, 11, 12, 13, 14, 15, 16, 17, 18}
	drives2 := []string{"d0", "d1"}

	tests := []struct {
		name       string
		plan       TapePlan
		drives     []string
		blankSlots []int
		// wantSets is the expected partition as (tapeIndex, copyIndex) pairs per
		// set; nil when the call is expected to fail.
		wantSets [][][2]int
		wantErr  require.ErrorAssertionFunc
	}{
		{
			// AC1: more logical tapes than drives, copies ≤ drives — one set per
			// logical tape, each holding that tape's two copies.
			name:       "extra logical tapes: 3 tapes, 2 copies, 2 drives",
			plan:       tapesFor(3, 2),
			drives:     drives2,
			blankSlots: blankSlots,
			wantSets: [][][2]int{
				{{0, 0}, {0, 1}},
				{{1, 0}, {1, 1}},
				{{2, 0}, {2, 1}},
			},
		},
		{
			// AC2: copy count exceeds the drive count — the copies of the one
			// logical tape spill into successive drive-sets, two at a time.
			name:       "extra copies: 1 tape, 4 copies, 2 drives",
			plan:       tapesFor(1, 4),
			drives:     drives2,
			blankSlots: blankSlots,
			wantSets: [][][2]int{
				{{0, 0}, {0, 1}},
				{{0, 2}, {0, 3}},
			},
		},
		{
			// Both extra logical tapes and extra copies at once: a single drive
			// makes every physical tape its own set.
			name:       "single drive: 2 tapes, 2 copies",
			plan:       tapesFor(2, 2),
			drives:     []string{"d0"},
			blankSlots: blankSlots,
			wantSets: [][][2]int{
				{{0, 0}}, {{0, 1}}, {{1, 0}}, {{1, 1}},
			},
		},
		{
			// A partial trailing set: pairs that do not fill a whole set still form
			// one (2 tapes × 1 copy across 2 drives is exactly full; add a third to
			// force a remainder).
			name:       "partial trailing set: 3 tapes, 1 copy, 2 drives",
			plan:       tapesFor(3, 1),
			drives:     drives2,
			blankSlots: blankSlots,
			wantSets: [][][2]int{
				{{0, 0}, {1, 0}},
				{{2, 0}},
			},
		},
		{
			name:       "copies equal drives: 1 tape, 2 copies, 2 drives",
			plan:       tapesFor(1, 2),
			drives:     drives2,
			blankSlots: blankSlots,
			wantSets: [][][2]int{
				{{0, 0}, {0, 1}},
			},
		},
		{
			name:       "empty plan yields no sets",
			plan:       TapePlan{Copies: 2},
			drives:     drives2,
			blankSlots: blankSlots,
			wantSets:   nil,
		},
		{
			name:       "not enough blank slots",
			plan:       tapesFor(2, 2), // 4 physical tapes
			drives:     drives2,
			blankSlots: []int{10, 11, 12}, // only 3
			wantErr:    require.Error,
		},
		{
			name:       "no drives configured",
			plan:       tapesFor(1, 1),
			drives:     nil,
			blankSlots: blankSlots,
			wantErr:    require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			wantErr := test.wantErr
			if wantErr == nil {
				wantErr = require.NoError
			}

			sets, err := planDriveSets(test.plan, test.drives, test.blankSlots)
			wantErr(t, err)

			if err != nil {
				return
			}

			// The partition matches the expected (tape, copy) pairs per set. Leave
			// got nil for an empty plan so it compares equal to a nil wantSets.
			var got [][][2]int
			if len(sets) > 0 {
				got = make([][][2]int, len(sets))
			}

			for i, set := range sets {
				got[i] = make([][2]int, len(set))
				for j, assignment := range set {
					got[i][j] = [2]int{assignment.TapeIndex, assignment.CopyIndex}
				}
			}

			assert.Equal(t, test.wantSets, got)

			// Invariants that every valid partition must hold.
			usedSlots := make(map[int]bool)

			for _, set := range sets {
				// A set never exceeds the drive count (AC3: bounded concurrency).
				assert.LessOrEqual(t, len(set), len(test.drives),
					"a drive-set must not exceed the drive count")

				for driveIdx, assignment := range set {
					// Drives map one-to-one onto the library's drives, in order, and
					// are reused across sets.
					assert.Equal(t, test.drives[driveIdx], assignment.Drive,
						"assignment must use the drive at its set position")
					// Every physical tape claims a distinct blank slot.
					assert.False(t, usedSlots[assignment.BlankSlot],
						"blank slot %d assigned twice", assignment.BlankSlot)
					usedSlots[assignment.BlankSlot] = true
				}
			}
		})
	}
}
