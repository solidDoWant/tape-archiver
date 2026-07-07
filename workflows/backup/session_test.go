package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
