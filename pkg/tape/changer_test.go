package tape

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAddressing mirrors the mhvtl L700 / production-style element address
// assignment page (0x1D): firstStorage=1000/47, firstIE=10/3, firstDTE=500/2,
// firstMTE=1/1. Friendly numbers derived from it match what mtx reported, so
// blankSlots configs stay byte-compatible.
func testAddressing() *elementAddressing {
	return &elementAddressing{
		firstMTE: 1, numMTE: 1,
		firstStorage: 1000, numStorage: 47,
		firstIE: 10, numIE: 3,
		firstDTE: 500, numDTE: 2,
	}
}

// --- MODE SENSE page 0x1D decoding ---------------------------------------

func TestParseElementAddressing(t *testing.T) {
	t.Parallel()

	// Captured verbatim from `MODE SENSE(6) page 0x1D` on the mhvtl L700: a
	// 4-byte header (block descriptor length 0) then the page.
	live := []byte{
		0x17, 0x00, 0x10, 0x00, // mode parameter header
		0x1d, 0x12, // page code 0x1D, page length 18
		0x00, 0x01, // first medium transport element = 1
		0x00, 0x01, // number of medium transport elements = 1
		0x03, 0xe8, // first storage element = 1000
		0x00, 0x2f, // number of storage elements = 47
		0x00, 0x0a, // first import/export element = 10
		0x00, 0x03, // number of import/export elements = 3
		0x01, 0xf4, // first data transfer element = 500
		0x00, 0x02, // number of data transfer elements = 2
		0x00, 0x00, // reserved
	}

	tests := []struct {
		name    string
		data    []byte
		want    *elementAddressing
		wantErr require.ErrorAssertionFunc
	}{
		{
			name:    "live mhvtl response",
			data:    live,
			want:    testAddressing(),
			wantErr: require.NoError,
		},
		{
			name: "block descriptors are skipped via header length",
			// Same page shifted by an 8-byte block descriptor the header declares.
			data: []byte{
				0x1f, 0x00, 0x10, 0x08, // header, block descriptor length = 8
				0, 0, 0, 0, 0, 0, 0, 0, // block descriptor (ignored)
				0x1d, 0x12,
				0x00, 0x01, 0x00, 0x01,
				0x03, 0xe8, 0x00, 0x2f,
				0x00, 0x0a, 0x00, 0x03,
				0x01, 0xf4, 0x00, 0x02,
				0x00, 0x00,
			},
			want:    testAddressing(),
			wantErr: require.NoError,
		},
		{
			name:    "short response",
			data:    []byte{0x00, 0x06, 0x00},
			wantErr: require.Error,
		},
		{
			name:    "unexpected page code",
			data:    []byte{0x0a, 0x00, 0x00, 0x00, 0x1a, 0x12, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			wantErr: require.Error,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseElementAddressing(tc.data)
			tc.wantErr(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, tc.want, got)
		})
	}
}

// --- READ ELEMENT STATUS decoding ----------------------------------------

const testDescLen = 52

// descriptor builds a single element descriptor of the standard 52-byte length.
func descriptor(addr int, flags byte, opts ...func([]byte)) []byte {
	d := make([]byte, testDescLen)
	d[descAddressOffset], d[descAddressOffset+1] = byte(addr>>8), byte(addr)
	d[descFlagsOffset] = flags

	for _, opt := range opts {
		opt(d)
	}

	return d
}

// withVolumeTag writes a space-padded primary volume tag at descriptor byte 12.
func withVolumeTag(tag string) func([]byte) {
	return func(d []byte) {
		for i := 0; i < primaryVolumeTagLen; i++ {
			if i < len(tag) {
				d[primaryVolumeTagOffset+i] = tag[i]
			} else {
				d[primaryVolumeTagOffset+i] = ' '
			}
		}
	}
}

// withSource sets SVALID and the raw source storage element address.
func withSource(raw int) func([]byte) {
	return func(d []byte) {
		d[descSVALIDOffset] = svalidBit
		d[descSourceOffset], d[descSourceOffset+1] = byte(raw>>8), byte(raw)
	}
}

// statusPage builds an element status page for one element type.
func statusPage(elementType byte, pvoltag bool, descs ...[]byte) []byte {
	page := make([]byte, elementStatusPageHeaderLen)
	page[0] = elementType

	if pvoltag {
		page[pageFlagsOffset] = pvoltagBit
	}

	page[pageDescLenOffset], page[pageDescLenOffset+1] = byte(testDescLen>>8), byte(testDescLen)

	byteCount := len(descs) * testDescLen
	page[pageByteCountOffset], page[pageByteCountOffset+1], page[pageByteCountOffset+2] =
		byte(byteCount>>16), byte(byteCount>>8), byte(byteCount)

	for _, d := range descs {
		page = append(page, d...)
	}

	return page
}

// elementStatus wraps pages in the status data header. Its internal fields are
// cosmetic here — the decoder walks pages by length from elementStatusHeaderLen
// on — so only the header size matters.
func elementStatus(pages ...[]byte) []byte {
	header := make([]byte, elementStatusHeaderLen)

	var body []byte
	for _, p := range pages {
		body = append(body, p...)
	}

	return append(header, body...)
}

const (
	flagFull       = elementFlagFull   // 0x01
	flagAccess     = elementFlagAccess // 0x08
	flagFullAccess = elementFlagFull | elementFlagAccess
)

func TestDecodeElementStatus(t *testing.T) {
	t.Parallel()

	// A full topology: 1 robot, 2 storage slots (one loaded), 2 drives (drive 1
	// loaded from raw storage 1004), and 3 I/O slots exercising the ACCESS bit in
	// both states. Element types intentionally out of order to prove the decoder
	// keys on the page type, not position.
	data := elementStatus(
		statusPage(elementTypeMediumTransport, false, descriptor(1, 0x00)),
		statusPage(elementTypeStorage, true,
			descriptor(1000, flagFullAccess, withVolumeTag("TA0001L6")),
			descriptor(1001, flagAccess),
		),
		statusPage(elementTypeDataTransfer, true,
			descriptor(500, flagAccess),
			descriptor(501, flagFullAccess, withVolumeTag("TA0005L6"), withSource(1004)),
		),
		statusPage(elementTypeImportExport, true,
			descriptor(10, flagFullAccess, withVolumeTag("TA0001L6")), // door closed, full
			descriptor(11, flagFull),                                  // door OPEN (ACCESS clear), full
			descriptor(12, flagAccess),                                // door closed, empty
		),
	)

	inv, err := decodeElementStatus(data, testAddressing())
	require.NoError(t, err)

	assert.Equal(t, []DriveElement{
		{Address: 0, Loaded: false},
		{Address: 1, Loaded: true, Barcode: "TA0005L6", SourceSlot: 5},
	}, inv.Drives, "drives")

	assert.Equal(t, []StorageElement{
		{Address: 1, Full: true, Barcode: "TA0001L6"},
		{Address: 2, Full: false},
	}, inv.Slots, "storage slots")

	assert.Equal(t, []IOElement{
		{Address: 48, Full: true, Barcode: "TA0001L6", Accessible: true},
		{Address: 49, Full: true, Barcode: "", Accessible: false},
		{Address: 50, Full: false, Accessible: true},
	}, inv.IOSlots, "I/O slots")

	assert.True(t, inv.IOAccessReported, "IO access reported")
}

func TestDecodeElementStatusNoIOStation(t *testing.T) {
	t.Parallel()

	// A library with no import/export elements reports no access state.
	data := elementStatus(
		statusPage(elementTypeStorage, true, descriptor(1000, flagFullAccess, withVolumeTag("TA0001L6"))),
	)

	inv, err := decodeElementStatus(data, testAddressing())
	require.NoError(t, err)

	assert.Empty(t, inv.IOSlots)
	assert.False(t, inv.IOAccessReported, "no I/O station means no access reported")
}

func TestDecodeElementStatusShort(t *testing.T) {
	t.Parallel()

	_, err := decodeElementStatus([]byte{0x00, 0x01}, testAddressing())
	require.Error(t, err)
}

// --- friendly ↔ raw address mapping --------------------------------------

func TestAddressingRawTranslation(t *testing.T) {
	t.Parallel()

	a := testAddressing()

	t.Run("storage", func(t *testing.T) {
		t.Parallel()

		raw, err := a.rawStorage(1)
		require.NoError(t, err)
		assert.Equal(t, 1000, raw)

		raw, err = a.rawStorage(47)
		require.NoError(t, err)
		assert.Equal(t, 1046, raw)

		_, err = a.rawStorage(0)
		assert.Error(t, err)

		_, err = a.rawStorage(48)
		assert.Error(t, err)
	})

	t.Run("drive", func(t *testing.T) {
		t.Parallel()

		raw, err := a.rawDrive(0)
		require.NoError(t, err)
		assert.Equal(t, 500, raw)

		raw, err = a.rawDrive(1)
		require.NoError(t, err)
		assert.Equal(t, 501, raw)

		_, err = a.rawDrive(2)
		assert.Error(t, err)

		_, err = a.rawDrive(-1)
		assert.Error(t, err)
	})

	t.Run("slot (combined storage + IO numbering)", func(t *testing.T) {
		t.Parallel()

		// 1..47 are storage; 48..50 are the I/O station.
		raw, err := a.rawSlot(1)
		require.NoError(t, err)
		assert.Equal(t, 1000, raw)

		raw, err = a.rawSlot(48)
		require.NoError(t, err)
		assert.Equal(t, 10, raw, "first I/O slot maps to firstIE")

		raw, err = a.rawSlot(50)
		require.NoError(t, err)
		assert.Equal(t, 12, raw)

		_, err = a.rawSlot(51)
		assert.Error(t, err, "beyond the last I/O slot")

		_, err = a.rawSlot(0)
		assert.Error(t, err)
	})

	t.Run("transport element", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, 1, a.transportElement())

		none := &elementAddressing{numMTE: 0}
		assert.Equal(t, 0, none.transportElement(), "no transport element falls back to 0")
	})
}

func TestFriendlyTranslationRoundTrip(t *testing.T) {
	t.Parallel()

	a := testAddressing()

	assert.Equal(t, 0, a.friendlyDrive(500))
	assert.Equal(t, 1, a.friendlyDrive(501))
	assert.Equal(t, 1, a.friendlyStorage(1000))
	assert.Equal(t, 47, a.friendlyStorage(1046))
	assert.Equal(t, 48, a.friendlyIO(10))
	assert.Equal(t, 50, a.friendlyIO(12))

	assert.True(t, a.isStorageRaw(1000))
	assert.True(t, a.isStorageRaw(1046))
	assert.False(t, a.isStorageRaw(999))
	assert.False(t, a.isStorageRaw(1047))
}

// --- volume tag trimming --------------------------------------------------

func TestTrimVolumeTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		field []byte
		want  Barcode
	}{
		{name: "space padded", field: []byte("TA0001L6" + "                        "), want: "TA0001L6"},
		{name: "null terminated", field: append([]byte("TA0042L6"), 0x00, 'x', 'y'), want: "TA0042L6"},
		{name: "all spaces is empty", field: []byte("        "), want: ""},
		{name: "empty", field: []byte{}, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, trimVolumeTag(tc.field))
		})
	}
}
