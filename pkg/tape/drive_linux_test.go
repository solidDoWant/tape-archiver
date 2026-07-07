//go:build linux

package tape

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInterpretBlankProbe exercises the blank-probe decision half of IsBlank
// directly. mhvtl cannot produce a status-only completion, so the status gate
// that keeps a transient BUSY/RESERVATION CONFLICT from becoming a definitive
// "not blank" verdict is only reachable via unit test.
func TestInterpretBlankProbe(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		probe     blankProbe
		wantBlank bool
		assertErr require.ErrorAssertionFunc
		wantErrIs error // errors.Is target the error must match (when erroring)
	}{
		"data present is not blank": {
			probe:     blankProbe{status: statusGood, transferred: 65536},
			wantBlank: false,
			assertErr: require.NoError,
		},
		"BLANK CHECK is blank": {
			probe:     blankProbe{status: statusCheckCondition, transferred: 0, senseKey: senseKeyBlankCheck},
			wantBlank: true,
			assertErr: require.NoError,
		},
		"filemark is not blank": {
			probe:     blankProbe{status: statusCheckCondition, transferred: 0, senseKey: senseKeyNoSense, filemark: true},
			wantBlank: false,
			assertErr: require.NoError,
		},
		"unexpected sense key errors": {
			probe:     blankProbe{status: statusCheckCondition, transferred: 0, senseKey: 0x03, asc: 0x11, ascq: 0x00},
			assertErr: require.Error,
			wantErrIs: errUnexpectedSense,
		},
		"BUSY status-only completion errors, not a false not-blank": {
			// The core bug: status-only completion, LLD leaves resid 0 so
			// transferred>0 on a genuinely blank tape. Must be an error (so the
			// ready-retry loop retries), never a definitive (false, nil).
			probe:     blankProbe{status: 0x08, transferred: 65536},
			assertErr: require.Error,
			wantErrIs: errUnexpectedSense,
		},
		"RESERVATION CONFLICT status-only completion errors": {
			probe:     blankProbe{status: 0x18, transferred: 65536},
			assertErr: require.Error,
			wantErrIs: errUnexpectedSense,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			blank, err := interpretBlankProbe(test.probe, "/dev/sg-test")

			test.assertErr(t, err)

			if err != nil {
				assert.False(t, blank, "no blank verdict may accompany an error")

				if test.wantErrIs != nil {
					assert.ErrorIs(t, err, test.wantErrIs)
				}

				return
			}

			assert.Equal(t, test.wantBlank, blank)
		})
	}
}
