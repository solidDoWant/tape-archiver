package backup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

func inventoryWith(drives []tape.DriveElement, slots []tape.StorageElement, ioSlots []tape.IOElement) tape.Inventory {
	return tape.Inventory{Drives: drives, Slots: slots, IOSlots: ioSlots}
}

func TestFormatSlotRanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		addresses []int
		want      string
	}{
		{name: "empty reads as none", addresses: nil, want: "none"},
		{name: "single slot", addresses: []int{7}, want: "7"},
		{name: "one contiguous run collapses", addresses: []int{1, 2, 3, 4}, want: "1-4"},
		{name: "disjoint runs are separated", addresses: []int{1, 2, 3, 7, 8, 20}, want: "1-3, 7-8, 20"},
		{name: "all isolated", addresses: []int{2, 4, 6}, want: "2, 4, 6"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, formatSlotRanges(test.addresses))
		})
	}
}

func TestOccupiedStorageSlots(t *testing.T) {
	t.Parallel()

	// Slot 3 is empty in the snapshot but holds a tape relocated during this
	// Load, so it must still count as occupied; slot 2 is genuinely empty.
	inv := inventoryWith(nil, []tape.StorageElement{
		{Address: 1, Full: true},
		{Address: 2, Full: false},
		{Address: 3, Full: false},
	}, nil)

	got := occupiedStorageSlots(inv, map[int]tape.Barcode{3: "RELOC3"})

	assert.Equal(t, []int{1, 3}, got)
}

// TestReconcileLoadEmptyTargetSlotErrorNamesWhereTapesAre: when a configured
// blank slot is empty, the error must say where the tapes are.
func TestReconcileLoadEmptyTargetSlotErrorNamesWhereTapesAre(t *testing.T) {
	t.Parallel()

	inv := inventoryWith(
		[]tape.DriveElement{{Address: 0, Loaded: true, Barcode: "TA0005L6", SourceSlot: 5}},
		[]tape.StorageElement{
			{Address: 1, Full: true},
			{Address: 2, Full: true},
			{Address: 3, Full: true},
			{Address: 20, Full: false}, // the configured blank slot, now empty
		},
		[]tape.IOElement{{Address: 48, Full: true}, {Address: 49, Full: false}},
	)

	_, err := reconcileLoad(t.Context(), nil, inv, tape.DriveElement{Address: 1}, 20, make(map[int]tape.Barcode))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "target slot 20 is empty")
	assert.Contains(t, err.Error(), "storage slots holding tapes: 1-3",
		"the error must name the slots that actually hold tapes")
	assert.Contains(t, err.Error(), "tapes in drives: 1")
	assert.Contains(t, err.Error(), "tapes in I/O station: 1")
}

func TestFindFreeStorageSlot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		slots      []tape.StorageElement
		preferSlot int
		relocated  map[int]tape.Barcode
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
		{
			name: "skips a claimed free slot and returns the next free one",
			slots: []tape.StorageElement{
				{Address: 1, Full: false},
				{Address: 2, Full: false},
				{Address: 3, Full: false},
			},
			preferSlot: 0,
			relocated:  map[int]tape.Barcode{1: "RELOC1"},
			want:       2,
		},
		{
			name: "skips the preferred slot when it is claimed",
			slots: []tape.StorageElement{
				{Address: 1, Full: false},
				{Address: 2, Full: false},
				{Address: 3, Full: false},
			},
			preferSlot: 2,
			relocated:  map[int]tape.Barcode{2: "RELOC2"},
			want:       1,
		},
		{
			name: "returns -1 when every free slot is claimed",
			slots: []tape.StorageElement{
				{Address: 1, Full: true},
				{Address: 2, Full: false},
			},
			preferSlot: 2,
			relocated:  map[int]tape.Barcode{2: "RELOC2"},
			want:       -1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			inv := inventoryWith(nil, test.slots, nil)
			got := findFreeStorageSlot(inv, test.preferSlot, test.relocated)
			assert.Equal(t, test.want, got)
		})
	}
}

// TestFindFreeStorageSlotDistinctPerClaim: a claimed slot is not offered again
// from the same snapshot within one Load.
func TestFindFreeStorageSlotDistinctPerClaim(t *testing.T) {
	t.Parallel()

	// Both drives report SourceSlot 0 (SVALID unreported), the production
	// collision case.
	inv := inventoryWith(nil, []tape.StorageElement{
		{Address: 1, Full: false},
		{Address: 2, Full: false},
	}, nil)

	relocated := make(map[int]tape.Barcode)

	first := findFreeStorageSlot(inv, 0, relocated)
	require.NotEqual(t, -1, first)
	relocated[first] = "RELOC"

	second := findFreeStorageSlot(inv, 0, relocated)
	require.NotEqual(t, -1, second)

	assert.NotEqual(t, first, second, "second relocation must not reuse the first's slot")
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

	baseSlots := []tape.StorageElement{
		{Address: 1, Barcode: "AAA001L8", Full: true},
		{Address: 2, Barcode: "BBB002L8", Full: true},
		{Address: 3, Full: false},
	}

	tests := []struct {
		name       string
		drive      tape.DriveElement
		targetSlot int
		// wantBarcode is empty when the call is expected to fail.
		wantBarcode tape.Barcode
		wantErr     require.ErrorAssertionFunc
	}{
		{
			name:        "drive empty loads from target slot",
			drive:       tape.DriveElement{Address: 0, Loaded: false},
			targetSlot:  1,
			wantBarcode: "AAA001L8",
			wantErr:     require.Error, // no real changer; expect error from changer.Load
		},
		{
			name:        "drive already loaded from target slot is idempotent",
			drive:       tape.DriveElement{Address: 0, Loaded: true, Barcode: "AAA001L8", SourceSlot: 1},
			targetSlot:  1,
			wantBarcode: "AAA001L8",
			wantErr:     require.NoError,
		},
		{
			name:       "target slot empty returns error",
			drive:      tape.DriveElement{Address: 0, Loaded: false},
			targetSlot: 3, // slot 3 is empty in baseSlots
			wantErr:    require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			inv := inventoryWith([]tape.DriveElement{test.drive}, baseSlots, nil)
			// The idempotent path issues no changer command; the others fail on the
			// command, which is the expected outcome.
			changer := tape.NewChanger("")
			bc, err := reconcileLoad(t.Context(), changer, inv, test.drive, test.targetSlot, make(map[int]tape.Barcode))
			test.wantErr(t, err)

			if err == nil {
				assert.Equal(t, test.wantBarcode, bc)
			}
		})
	}
}

// errNotReady stands in for the NOT READY a real drive returns while threading
// a freshly loaded tape.
var errNotReady = errors.New("SCSI NOT READY - becoming ready")

// fakeBlankChecker fails its first notReadyN calls (all of them with
// alwaysErr), then returns blank, recording calls.
type fakeBlankChecker struct {
	notReadyN int   // number of leading calls that return the not-ready error
	blank     bool  // verdict returned once the drive is "ready"
	alwaysErr bool  // drive never becomes ready — every call returns the error
	err       error // error returned while not ready (defaults to errNotReady)
	calls     int   // total IsBlank invocations, for retry assertions
}

func (f *fakeBlankChecker) IsBlank(context.Context) (bool, error) {
	f.calls++

	notReadyErr := f.err
	if notReadyErr == nil {
		notReadyErr = errNotReady
	}

	if f.alwaysErr || f.calls <= f.notReadyN {
		return false, notReadyErr
	}

	return f.blank, nil
}

func TestBlankCheckWhenReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		fake      *fakeBlankChecker
		wantBlank bool
		wantCalls int
	}{
		{
			name:      "ready immediately reports blank",
			fake:      &fakeBlankChecker{notReadyN: 0, blank: true},
			wantBlank: true,
			wantCalls: 1,
		},
		{
			name:      "ready immediately reports non-blank",
			fake:      &fakeBlankChecker{notReadyN: 0, blank: false},
			wantBlank: false,
			wantCalls: 1,
		},
		{
			// AC1: wantCalls proves the loop retried rather than returning the first
			// error.
			name:      "blank after transient NOT READY",
			fake:      &fakeBlankChecker{notReadyN: 3, blank: true},
			wantBlank: true,
			wantCalls: 4,
		},
		{
			name:      "non-blank after transient NOT READY",
			fake:      &fakeBlankChecker{notReadyN: 3, blank: false},
			wantBlank: false,
			wantCalls: 4,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			blank, err := blankCheckWhenReady(t.Context(), test.fake, time.Minute, time.Millisecond)
			require.NoError(t, err)
			assert.Equal(t, test.wantBlank, blank)
			assert.Equal(t, test.wantCalls, test.fake.calls,
				"retry loop must poll through every NOT-READY error before returning the verdict")
		})
	}
}

// TestBlankCheckWhenReadyDeadlineExpires: a never-ready drive surfaces its last
// error after retrying at least once.
func TestBlankCheckWhenReadyDeadlineExpires(t *testing.T) {
	t.Parallel()

	driveErr := errors.New("persistent hardware fault")
	fake := &fakeBlankChecker{alwaysErr: true, err: driveErr}

	blank, err := blankCheckWhenReady(t.Context(), fake, 40*time.Millisecond, 5*time.Millisecond)

	require.ErrorIs(t, err, driveErr)
	assert.False(t, blank)
	assert.GreaterOrEqual(t, fake.calls, 2,
		"must retry at least once before the deadline, not return the first error")
}

// TestBlankCheckWhenReadyReturnsOnContextCancelDuringPoll: cancellation ends
// the poll wait promptly. The poll is too long to fire, so only ctx.Done() can
// unblock it.
func TestBlankCheckWhenReadyReturnsOnContextCancelDuringPoll(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	driveErr := errors.New("NOT READY during pause")
	fake := &fakeBlankChecker{alwaysErr: true, err: driveErr}

	time.AfterFunc(20*time.Millisecond, cancel)

	start := time.Now()
	blank, err := blankCheckWhenReady(ctx, fake, time.Minute, 30*time.Second)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, driveErr)
	assert.False(t, blank)
	assert.Less(t, elapsed, 5*time.Second,
		"must return promptly via the ctx.Done() select arm, not wait out the poll interval")
}

// TestReconcileLoadClaimsDistinctSlots (AC2): two drives with unexpected tapes
// reporting SourceSlot 0 must claim distinct relocation slots from one shared
// snapshot.
func TestReconcileLoadClaimsDistinctSlots(t *testing.T) {
	t.Parallel()

	slots := []tape.StorageElement{
		{Address: 1, Full: false},
		{Address: 2, Full: false},
		{Address: 5, Barcode: "BLNK01L8", Full: true},
	}
	drives := []tape.DriveElement{
		{Address: 0, Loaded: true, Barcode: "UNEXP0L8", SourceSlot: 0},
		{Address: 1, Loaded: true, Barcode: "UNEXP1L8", SourceSlot: 0},
	}

	inv := inventoryWith(drives, slots, nil)
	changer := tape.NewChanger("") // relocate's unload fails, but only after the slot is claimed
	relocated := make(map[int]tape.Barcode)

	// Both fail at the unload (no real changer), after claiming a slot.
	_, err := reconcileLoad(t.Context(), changer, inv, drives[0], 5, relocated)
	require.Error(t, err)

	_, err = reconcileLoad(t.Context(), changer, inv, drives[1], 5, relocated)
	require.Error(t, err)

	assert.Len(t, relocated, 2, "each relocation must claim its own storage slot")
	assert.Equal(t, tape.Barcode("UNEXP0L8"), relocated[1], "first relocation should claim slot 1")
	assert.Equal(t, tape.Barcode("UNEXP1L8"), relocated[2], "second relocation should claim the distinct slot 2")
}

// TestReconcileLoadResolvesRelocatedTargetSlot (#224 AC3): a blank relocated
// back into a later assignment's slot must be found there, not reported empty
// off the stale snapshot. The nil changer fails the moves, but the relocation is
// recorded first, so the later call reaches the load for the right barcode.
func TestReconcileLoadResolvesRelocatedTargetSlot(t *testing.T) {
	t.Parallel()

	// Slot 5 looks empty because its tape (BLNK05L8) is in drive 0.
	slots := []tape.StorageElement{
		{Address: 1, Barcode: "BLNK01L8", Full: true},
		{Address: 5, Full: false},
	}
	driveZero := tape.DriveElement{Address: 0, Loaded: true, Barcode: "BLNK05L8", SourceSlot: 5}
	driveOne := tape.DriveElement{Address: 1, Loaded: false}

	inv := inventoryWith([]tape.DriveElement{driveZero, driveOne}, slots, nil)
	changer := tape.NewChanger("") // physical moves fail, but only after the relocation is recorded
	relocated := make(map[int]tape.Barcode)

	_, err := reconcileLoad(t.Context(), changer, inv, driveZero, 1, relocated)
	require.Error(t, err) // fails at the (nil-changer) unload, after recording the relocation
	require.Equal(t, tape.Barcode("BLNK05L8"), relocated[5],
		"relocating drive 0's unexpected tape must record its barcode against home slot 5")

	_, err = reconcileLoad(t.Context(), changer, inv, driveOne, 5, relocated)
	require.Error(t, err) // fails at the (nil-changer) load, i.e. it got past the empty-slot check
	assert.NotContains(t, err.Error(), "is empty",
		"the relocated blank in the target slot must be seen, not reported empty")
	assert.ErrorContains(t, err, "load tape BLNK05L8 from slot 5",
		"reconcile must reach the load command for the relocated blank in the target slot")
}

// TestMatchDriveElement covers identity pairing (#137): unit serial selects the
// element with the same DVCID serial regardless of order; unidentifiable or
// ambiguous drives fail.
func TestMatchDriveElement(t *testing.T) {
	t.Parallel()

	// Probe-order mismatch: DTE order is the reverse of the config's drive order.
	swapped := []tape.DriveElement{
		{Address: 0, Serial: "SN_B"},
		{Address: 1, Serial: "SN_A"},
	}

	tests := []struct {
		name        string
		drives      []tape.DriveElement
		serial      string
		wantAddress int
		wantErr     require.ErrorAssertionFunc
	}{
		{
			name:        "serial selects the element regardless of position",
			drives:      swapped,
			serial:      "SN_A",
			wantAddress: 1,
			wantErr:     require.NoError,
		},
		{
			name:        "the other serial selects the other element",
			drives:      swapped,
			serial:      "SN_B",
			wantAddress: 0,
			wantErr:     require.NoError,
		},
		{
			name:    "empty serial cannot be paired",
			drives:  swapped,
			serial:  "",
			wantErr: require.Error,
		},
		{
			name:    "no matching element",
			drives:  swapped,
			serial:  "SN_C",
			wantErr: require.Error,
		},
		{
			name: "ambiguous serial is rejected",
			drives: []tape.DriveElement{
				{Address: 0, Serial: "SN_DUP"},
				{Address: 1, Serial: "SN_DUP"},
			},
			serial:  "SN_DUP",
			wantErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			de, err := matchDriveElement(inventoryWith(test.drives, nil, nil), test.serial)
			test.wantErr(t, err)

			if err == nil {
				assert.Equal(t, test.wantAddress, de.Address)
			}
		})
	}
}

func tapesFor(tapeCount, copies int) TapePlan {
	tapes := make([]PlannedTape, tapeCount)
	for i := range tapes {
		tapes[i] = PlannedTape{Archives: []PlannedArchive{{SourceIndex: 0}}}
	}

	return TapePlan{Copies: copies, Tapes: tapes}
}

func TestPlanDriveSets(t *testing.T) {
	t.Parallel()

	blankSlots := []int{10, 11, 12, 13, 14, 15, 16, 17, 18}
	drives2 := []string{"d0", "d1"}

	tests := []struct {
		name       string
		plan       TapePlan
		drives     []string
		blankSlots []int
		// wantSets is nil when the call is expected to fail.
		wantSets [][][2]int
		wantErr  require.ErrorAssertionFunc
	}{
		{
			// AC1: more logical tapes than drives.
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
			// AC2: more copies than drives spill into successive sets.
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
			name:       "single drive: 2 tapes, 2 copies",
			plan:       tapesFor(2, 2),
			drives:     []string{"d0"},
			blankSlots: blankSlots,
			wantSets: [][][2]int{
				{{0, 0}}, {{0, 1}}, {{1, 0}}, {{1, 1}},
			},
		},
		{
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

			// Leave got nil for an empty plan so it equals a nil wantSets.
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

			usedSlots := make(map[int]bool)

			for _, set := range sets {
				// AC3: bounded concurrency.
				assert.LessOrEqual(t, len(set), len(test.drives),
					"a drive-set must not exceed the drive count")

				for driveIdx, assignment := range set {
					assert.Equal(t, test.drives[driveIdx], assignment.Drive,
						"assignment must use the drive at its set position")
					assert.False(t, usedSlots[assignment.BlankSlot],
						"blank slot %d assigned twice", assignment.BlankSlot)
					usedSlots[assignment.BlankSlot] = true
				}
			}
		})
	}
}
