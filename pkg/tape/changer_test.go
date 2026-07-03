package tape

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		status         string
		wantAccessible bool
		wantReported   bool
	}{
		{name: "absent", status: "Full :VolumeTag=TA0001L6", wantReported: false},
		{name: "access=1 closed", status: "Empty:Access=1", wantAccessible: true, wantReported: true},
		{name: "access=0 open", status: "Empty:Access=0", wantAccessible: false, wantReported: true},
		{name: "spaces around equals", status: "Empty:Access = yes", wantAccessible: true, wantReported: true},
		{name: "closed keyword", status: "Full :VolumeTag=TA0001L6:Access=closed", wantAccessible: true, wantReported: true},
		{name: "open keyword", status: "Empty:Access=open", wantAccessible: false, wantReported: true},
		{name: "unrecognized value is reported but not accessible", status: "Empty:Access=maybe", wantAccessible: false, wantReported: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			accessible, reported := parseAccess(tc.status)
			assert.Equal(t, tc.wantReported, reported, "reported")
			assert.Equal(t, tc.wantAccessible, accessible, "accessible")
		})
	}
}

// allStorageSlots returns the expected StorageElement slice for the default
// mhvtl fixture where slots 1–47 are all loaded with barcodes TA0001L6–TA0047L6.
func allStorageSlots() []StorageElement {
	slots := make([]StorageElement, 47)
	for i := range slots {
		slots[i] = StorageElement{
			Address: i + 1,
			Full:    true,
			Barcode: Barcode(fmt.Sprintf("TA%04dL6", i+1)),
		}
	}

	return slots
}

func TestParseVolumeTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status string
		want   Barcode
	}{
		{
			name:   "storage form, no spaces",
			status: "Full :VolumeTag=TA0001L6",
			want:   "TA0001L6",
		},
		{
			name:   "drive form, spaces around equals",
			status: "Full (Storage Element 1 Loaded):VolumeTag = TA0001L6",
			want:   "TA0001L6",
		},
		{
			name:   "alternate volume tag must not bleed into barcode (storage)",
			status: "Full :VolumeTag=TA0001L6:AlternateVolumeTag=ALT123",
			want:   "TA0001L6",
		},
		{
			name:   "alternate volume tag must not bleed into barcode (drive)",
			status: "Full (Storage Element 1 Loaded):VolumeTag = TA0001L6:AlternateVolumeTag = ALT123",
			want:   "TA0001L6",
		},
		{
			name:   "no volume tag",
			status: "Empty",
			want:   "",
		},
		{
			name:   "alternate tag only is ignored",
			status: "Full :AlternateVolumeTag=ALT123",
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, parseVolumeTag(tc.status))
		})
	}
}

func TestParseInventory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		fixture              string
		wantDrives           []DriveElement
		wantSlots            []StorageElement
		wantIO               []IOElement
		wantIOAccessReported bool
		wantErr              require.ErrorAssertionFunc
	}{
		{
			name:    "all drives empty, all slots loaded",
			fixture: "testdata/mtx_status_all_empty.txt",
			wantDrives: []DriveElement{
				{Address: 0},
				{Address: 1},
			},
			wantSlots: allStorageSlots(),
			wantIO: []IOElement{
				{Address: 48},
				{Address: 49},
				{Address: 50},
			},
			wantErr: require.NoError,
		},
		{
			name:    "drive 0 loaded with TA0001L6 from slot 1",
			fixture: "testdata/mtx_status_drive0_loaded.txt",
			wantDrives: []DriveElement{
				{Address: 0, Loaded: true, Barcode: "TA0001L6", SourceSlot: 1},
				{Address: 1},
			},
			wantIO: []IOElement{
				{Address: 48},
				{Address: 49},
				{Address: 50},
			},
			wantErr: require.NoError,
		},
		{
			name:    "IO slot 48 full with TA0001L6",
			fixture: "testdata/mtx_status_io_full.txt",
			wantDrives: []DriveElement{
				{Address: 0},
				{Address: 1},
			},
			wantIO: []IOElement{
				{Address: 48, Full: true, Barcode: "TA0001L6"},
				{Address: 49},
				{Address: 50},
			},
			wantErr: require.NoError,
		},
		{
			name:    "IO station reports the access bit",
			fixture: "testdata/mtx_status_io_access.txt",
			wantDrives: []DriveElement{
				{Address: 0},
				{Address: 1},
			},
			wantIO: []IOElement{
				{Address: 48, Full: true, Barcode: "TA0001L6", Accessible: true},
				{Address: 49, Accessible: false},
				{Address: 50, Accessible: true},
			},
			wantIOAccessReported: true,
			wantErr:              require.NoError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(tc.fixture)
			require.NoError(t, err, "read fixture")

			inv, err := parseInventory(string(data))
			tc.wantErr(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, tc.wantDrives, inv.Drives, "drives")
			assert.Equal(t, tc.wantIO, inv.IOSlots, "IO slots")
			assert.Equal(t, tc.wantIOAccessReported, inv.IOAccessReported, "IO access reported")

			if tc.wantSlots != nil {
				assert.Equal(t, tc.wantSlots, inv.Slots, "storage slots")
			}
		})
	}
}
