package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// TestValidateBarcodes covers the pre-write barcode invariant (#170).
func TestValidateBarcodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		loaded         []LoadedTape
		requireErr     require.ErrorAssertionFunc
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

// TestMountRegistryUnmountState covers the bookkeeping that makes FinalizeTape
// retries idempotent (#152 AC3): MarkUnmounted keeps the entry, only Delete
// removes it.
func TestMountRegistryUnmountState(t *testing.T) {
	t.Parallel()

	registry := newMountRegistry()

	const device = "/dev/sg0"

	_, _, ok := registry.getEntry(device)
	assert.False(t, ok, "an unknown device has no entry")

	// nil mount: only the bookkeeping is exercised.
	registry.Put(device, nil)

	_, unmounted, ok := registry.getEntry(device)
	require.True(t, ok, "Put must register the entry")
	assert.False(t, unmounted, "a freshly parked mount must not be marked unmounted")

	registry.MarkUnmounted(device)

	_, unmounted, ok = registry.getEntry(device)
	require.True(t, ok, "the entry must survive MarkUnmounted so the retry is not mount-lost")
	assert.True(t, unmounted, "MarkUnmounted must record the unmount")

	registry.Delete(device)

	_, _, ok = registry.getEntry(device)
	assert.False(t, ok, "Delete must remove the entry")
}

// TestFinalizeTapeMountLostIsNonRetryable: an absent entry (only a data-worker
// restart causes one) fails fast and non-retryably (#152 AC3).
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
