//go:build linux

package tape

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixedSense(key, asc, ascq byte, filemark bool) []byte {
	sb := make([]byte, 18)
	sb[0] = 0x70

	sb[2] = key & 0x0f
	if filemark {
		sb[2] |= senseFilemarkBit
	}

	sb[12] = asc
	sb[13] = ascq

	return sb
}

// TestDecodeReadBlock covers status-only completions, which mhvtl cannot
// produce.
func TestDecodeReadBlock(t *testing.T) {
	t.Parallel()

	const bufLen = 4096

	// Non-zero so a real block is distinguishable from a fabricated zero block.
	dataBuf := make([]byte, bufLen)
	for i := range dataBuf {
		dataBuf[i] = byte(i%251) + 1
	}

	tests := map[string]struct {
		hdr       sgIOHdr
		sense     []byte
		assertErr require.ErrorAssertionFunc
		wantData  []byte // expected payload on success
		errSubstr string // substring the error must contain (when assertErr expects an error)
	}{
		"GOOD full data block": {
			hdr:       sgIOHdr{status: statusGood, dxferLen: bufLen, resid: 0},
			sense:     nil,
			assertErr: require.NoError,
			wantData:  dataBuf,
		},
		"CHECK CONDITION NO SENSE short block": {
			// LTFS short block: CHECK CONDITION, NO SENSE incorrect-length, partial resid.
			hdr:       sgIOHdr{status: statusCheckCondition, dxferLen: bufLen, resid: bufLen - 1000},
			sense:     fixedSense(senseKeyNoSense, 0x00, 0x00, false),
			assertErr: require.NoError,
			wantData:  dataBuf[:1000],
		},
		"CHECK CONDITION BLANK CHECK": {
			hdr:       sgIOHdr{status: statusCheckCondition, dxferLen: bufLen, resid: bufLen},
			sense:     fixedSense(senseKeyBlankCheck, 0x00, 0x05, false),
			assertErr: require.Error,
			errSubstr: "no data block",
		},
		"CHECK CONDITION filemark": {
			hdr:       sgIOHdr{status: statusCheckCondition, dxferLen: bufLen, resid: bufLen},
			sense:     fixedSense(senseKeyNoSense, 0x00, 0x01, true),
			assertErr: require.Error,
			errSubstr: "no data block",
		},
		"BUSY status-only completion, empty sense, zero resid": {
			// Status-only completion with resid 0 must not fabricate a block.
			hdr:       sgIOHdr{status: 0x08, dxferLen: bufLen, resid: 0},
			sense:     nil,
			assertErr: require.Error,
			errSubstr: "status=0x08",
		},
		"RESERVATION CONFLICT status-only completion": {
			hdr:       sgIOHdr{status: 0x18, dxferLen: bufLen, resid: 0},
			sense:     nil,
			assertErr: require.Error,
			errSubstr: "status=0x18",
		},
		"TASK SET FULL status-only completion": {
			hdr:       sgIOHdr{status: 0x28, dxferLen: bufLen, resid: 0},
			sense:     nil,
			assertErr: require.Error,
			errSubstr: "status=0x28",
		},
		"transport error host status": {
			hdr:       sgIOHdr{status: statusGood, hostStatus: 0x07, dxferLen: bufLen, resid: 0},
			sense:     nil,
			assertErr: require.Error,
			errSubstr: "transport error",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			buf := make([]byte, bufLen)
			copy(buf, dataBuf)

			out, err := decodeReadBlock(&test.hdr, test.sense, buf, "/dev/sg-test")

			test.assertErr(t, err)

			if err != nil {
				assert.Nil(t, out, "no payload may be returned on error")

				if test.errSubstr != "" {
					assert.ErrorContains(t, err, test.errSubstr)
				}

				return
			}

			assert.Equal(t, test.wantData, out)
		})
	}
}
