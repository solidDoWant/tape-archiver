package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// TestValidateBarcodes exercises the pure pre-write barcode invariant: every
// tape in a drive-set must carry a non-empty barcode and no two may share one
// (issue #170). Empty barcodes are named by drive index, duplicates by the
// shared barcode value and the drives that carry it; a valid set returns nil.
func TestValidateBarcodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		loaded     []LoadedTape
		requireErr require.ErrorAssertionFunc
		// wantSubstrings are fragments the joined error must name (offenders).
		wantSubstrings []string
	}{
		{
			name:       "empty set validates trivially",
			loaded:     nil,
			requireErr: require.NoError,
		},
		{
			name: "distinct non-empty barcodes pass",
			loaded: []LoadedTape{
				{Barcode: "BC-0", DriveIndex: 0},
				{Barcode: "BC-1", DriveIndex: 1},
			},
			requireErr: require.NoError,
		},
		{
			name: "single empty barcode is rejected by drive index",
			loaded: []LoadedTape{
				{Barcode: "", DriveIndex: 0},
				{Barcode: "BC-1", DriveIndex: 1},
			},
			requireErr:     require.Error,
			wantSubstrings: []string{"drive 0", "empty barcode"},
		},
		{
			name: "duplicate barcode is rejected naming value and drives",
			loaded: []LoadedTape{
				{Barcode: "BC-DUP", DriveIndex: 0},
				{Barcode: "BC-DUP", DriveIndex: 1},
			},
			requireErr:     require.Error,
			wantSubstrings: []string{"duplicate barcode", "BC-DUP", "[0 1]"},
		},
		{
			name: "mixed empty and duplicate offenders all reported",
			loaded: []LoadedTape{
				{Barcode: "", DriveIndex: 0},
				{Barcode: "BC-DUP", DriveIndex: 1},
				{Barcode: "BC-DUP", DriveIndex: 2},
				{Barcode: "", DriveIndex: 3},
			},
			requireErr: require.Error,
			wantSubstrings: []string{
				"drive 0", "drive 3", // both empties
				"duplicate barcode", "BC-DUP", "[1 2]", // the duplicate and its drives
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateBarcodes(test.loaded)

			test.requireErr(t, err)

			for _, want := range test.wantSubstrings {
				assert.ErrorContains(t, err, want)
			}
		})
	}
}

// TestValidateBarcodesTriplicate confirms a barcode shared by three drives lists
// all three drive indices in the error (single-pass encounter order).
func TestValidateBarcodesTriplicate(t *testing.T) {
	t.Parallel()

	err := validateBarcodes([]LoadedTape{
		{Barcode: tape.Barcode("SAME"), DriveIndex: 0},
		{Barcode: tape.Barcode("SAME"), DriveIndex: 1},
		{Barcode: tape.Barcode("SAME"), DriveIndex: 2},
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, "[0 1 2]")
}

// TestMountRegistryUnmountState covers the registry mechanism that makes
// FinalizeTape's retry idempotent across the unmount boundary (issue #152 AC3):
// a mount parked by WriteTree starts not-unmounted, MarkUnmounted flips the flag
// while keeping the entry present (so a retry is not misdiagnosed as a lost
// mount), and only Delete removes it.
func TestMountRegistryUnmountState(t *testing.T) {
	t.Parallel()

	registry := newMountRegistry()

	const device = "/dev/sg0"

	_, _, ok := registry.getEntry(device)
	assert.False(t, ok, "an unknown device has no entry")

	// A freshly parked mount is present and not yet unmounted. A nil *ltfs.Mount
	// is fine here: this test exercises only the entry's unmount-state bookkeeping.
	registry.Put(device, nil)

	_, unmounted, ok := registry.getEntry(device)
	require.True(t, ok, "Put must register the entry")
	assert.False(t, unmounted, "a freshly parked mount must not be marked unmounted")

	// MarkUnmounted flips the flag but keeps the entry, so a FinalizeTape retry
	// after a successful Unmount re-reads the captured index instead of hitting
	// the mount-lost branch and re-driving a finalized tape (SPEC §14).
	registry.MarkUnmounted(device)

	_, unmounted, ok = registry.getEntry(device)
	require.True(t, ok, "the entry must survive MarkUnmounted so the retry is not mount-lost")
	assert.True(t, unmounted, "MarkUnmounted must record the unmount")

	// Only a successful finalize deletes the entry.
	registry.Delete(device)

	_, _, ok = registry.getEntry(device)
	assert.False(t, ok, "Delete must remove the entry")
}

// TestFinalizeTapeMountLostIsNonRetryable covers the sole terminal case (issue
// #152 AC3): a genuinely absent registry entry — only ever caused by a data-worker
// restart that wiped the in-memory registry — fails fast and non-retryably rather
// than retrying an unrecoverable state until the session timeout.
func TestFinalizeTapeMountLostIsNonRetryable(t *testing.T) {
	t.Parallel()

	acts := newWriteActivities(newMountRegistry(), t.TempDir())

	_, err := acts.FinalizeTape(t.Context(), FinalizeInput{Device: "/dev/sg0"})
	require.Error(t, err)

	var appErr *temporal.ApplicationError
	require.ErrorAs(t, err, &appErr, "a lost mount must be a Temporal ApplicationError")
	assert.True(t, appErr.NonRetryable(), "a genuinely lost mount must fail fast, not retry")
	assert.Equal(t, "mount-lost", appErr.Type())
}
