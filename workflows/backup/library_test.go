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
		claimed    map[int]bool
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
			claimed:    map[int]bool{1: true},
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
			claimed:    map[int]bool{2: true},
			want:       1,
		},
		{
			name: "returns -1 when every free slot is claimed",
			slots: []tape.StorageElement{
				{Address: 1, Full: true},
				{Address: 2, Full: false},
			},
			preferSlot: 2,
			claimed:    map[int]bool{2: true},
			want:       -1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			inv := inventoryWith(nil, test.slots, nil)
			got := findFreeStorageSlot(inv, test.preferSlot, test.claimed)
			assert.Equal(t, test.want, got)
		})
	}
}

// TestFindFreeStorageSlotDistinctPerClaim proves the AC2 behavior at the
// slot-selection layer: choosing a slot, recording it as claimed, then choosing
// again from the same inventory snapshot yields a distinct slot — the previously
// chosen slot is not offered a second time within one Load.
func TestFindFreeStorageSlotDistinctPerClaim(t *testing.T) {
	t.Parallel()

	// Both drives report SourceSlot 0 (SVALID unreported), so both prefer the same
	// (absent) slot 0 and fall back to the free-slot scan — the exact production
	// collision this issue fixes.
	inv := inventoryWith(nil, []tape.StorageElement{
		{Address: 1, Full: false},
		{Address: 2, Full: false},
	}, nil)

	claimed := make(map[int]bool)

	first := findFreeStorageSlot(inv, 0, claimed)
	require.NotEqual(t, -1, first)
	claimed[first] = true

	second := findFreeStorageSlot(inv, 0, claimed)
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
			bc, err := reconcileLoad(t.Context(), changer, inv, test.driveIndex, test.driveAddr, test.targetSlot, make(map[int]bool))
			test.wantErr(t, err)

			if err == nil {
				assert.Equal(t, test.wantBarcode, bc)
			}
		})
	}
}

// errNotReady stands in for the SCSI NOT READY / BECOMING READY error a real
// drive returns while it is still threading and calibrating a freshly loaded
// tape — the transient failure blankCheckWhenReady must poll through.
var errNotReady = errors.New("SCSI NOT READY - becoming ready")

// fakeBlankChecker is a deterministic blankChecker for exercising
// blankCheckWhenReady's NOT-READY retry loop without a real drive or mhvtl. Its
// first notReadyN calls report a not-ready error, then it returns the blank
// verdict; with alwaysErr set it never becomes ready. It records calls so a test
// can prove the loop actually retried rather than returning the first error.
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
			// AC1: transient NOT READY, then the correct blank verdict once ready.
			// wantCalls proves the loop retried through every not-ready error rather
			// than returning the first one (the "first error returned immediately"
			// regression would return after a single call with an error).
			name:      "blank after transient NOT READY",
			fake:      &fakeBlankChecker{notReadyN: 3, blank: true},
			wantBlank: true,
			wantCalls: 4,
		},
		{
			// AC1: same retry loop must also surface a correct non-blank verdict.
			name:      "non-blank after transient NOT READY",
			fake:      &fakeBlankChecker{notReadyN: 3, blank: false},
			wantBlank: false,
			wantCalls: 4,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Tiny poll keeps the retry loop fast; a generous timeout ensures the
			// drive becomes ready well before the deadline.
			blank, err := blankCheckWhenReady(t.Context(), test.fake, time.Minute, time.Millisecond)
			require.NoError(t, err)
			assert.Equal(t, test.wantBlank, blank)
			assert.Equal(t, test.wantCalls, test.fake.calls,
				"retry loop must poll through every NOT-READY error before returning the verdict")
		})
	}
}

// TestBlankCheckWhenReadyDeadlineExpires covers the deadline bound: a drive that
// never becomes ready must surface its last error once the timeout elapses,
// having retried at least once first. A regression that inverts the deadline
// check would either return on the first error (calls == 1) or loop forever
// (tripping the go test timeout).
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

// TestBlankCheckWhenReadyReturnsOnContextCancelDuringPoll covers the poll
// select's ctx.Done() arm: while parked between retries, a cancelled context
// must end the wait promptly and surface the drive's last error. The poll is
// large so its timer cannot fire within the test — only the ctx.Done() arm can
// unblock the select. A regression that drops that arm would wait out the full
// poll (caught by the elapsed-time bound below).
func TestBlankCheckWhenReadyReturnsOnContextCancelDuringPoll(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	driveErr := errors.New("NOT READY during pause")
	fake := &fakeBlankChecker{alwaysErr: true, err: driveErr}

	// Cancel once the loop is parked in the poll select (well after the first,
	// microsecond-scale IsBlank call and the pre-poll guard).
	time.AfterFunc(20*time.Millisecond, cancel)

	start := time.Now()
	blank, err := blankCheckWhenReady(ctx, fake, time.Minute, 30*time.Second)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, driveErr)
	assert.False(t, blank)
	assert.Less(t, elapsed, 5*time.Second,
		"must return promptly via the ctx.Done() select arm, not wait out the poll interval")
}

// TestReconcileLoadClaimsDistinctSlots proves the fix for this issue at the
// reconcile layer (AC2): two drives that both hold an unexpected tape and both
// report SourceSlot 0 (SVALID unreported) must be offered distinct free storage
// slots when reconciled against one shared inventory snapshot. reconcileLoad
// records each chosen relocation slot in the shared claimed map before issuing
// the (here failing, no real changer) unload, so the second drive cannot be
// handed the slot the first already claimed. With the pre-fix code both drives
// would claim the same first free slot.
func TestReconcileLoadClaimsDistinctSlots(t *testing.T) {
	t.Parallel()

	// Two free storage slots (1, 2) and a target blank slot (5). Both drives hold
	// an unexpected tape with SourceSlot 0 — the deterministic collision case.
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
	claimed := make(map[int]bool)

	// Both calls fail at the unload (no real changer), but each must have claimed
	// its relocation slot first.
	_, err := reconcileLoad(t.Context(), changer, inv, 0, drives[0].Address, 5, claimed)
	require.Error(t, err)

	_, err = reconcileLoad(t.Context(), changer, inv, 1, drives[1].Address, 5, claimed)
	require.Error(t, err)

	// Exactly two distinct free slots claimed — no collision.
	assert.Len(t, claimed, 2, "each relocation must claim its own storage slot")
	assert.True(t, claimed[1], "first relocation should claim slot 1")
	assert.True(t, claimed[2], "second relocation should claim the distinct slot 2")
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
