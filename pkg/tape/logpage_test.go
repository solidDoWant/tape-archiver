package tape

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTapeAlert(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		wantAnySet  bool
		wantFlagSet map[int]bool // flag number → expected Set value (spot-checked)
		wantErr     require.ErrorAssertionFunc
	}{
		{
			name:       "all flags clear",
			fixture:    "testdata/sg_logs_tapealert_clean.json",
			wantAnySet: false,
			wantFlagSet: map[int]bool{
				0x01: false,
				0x02: false,
				0x04: false,
			},
			wantErr: require.NoError,
		},
		{
			name:       "write warning and media flags set",
			fixture:    "testdata/sg_logs_tapealert_flagged.json",
			wantAnySet: true,
			wantFlagSet: map[int]bool{
				0x01: false, // Read warning — clear
				0x02: true,  // Write warning — set
				0x03: false, // Hard error — clear
				0x04: true,  // Media — set
			},
			wantErr: require.NoError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(tc.fixture)
			require.NoError(t, err, "read fixture")

			result, err := parseTapeAlert(string(data))
			tc.wantErr(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, tc.wantAnySet, result.AnySet(), "AnySet()")
			assert.NotEmpty(t, result.Flags, "should have parsed at least one flag")

			flagByNum := make(map[int]TapeAlertFlag, len(result.Flags))
			for _, flag := range result.Flags {
				flagByNum[flag.Number] = flag
			}

			for num, wantSet := range tc.wantFlagSet {
				flag, ok := flagByNum[num]
				if assert.True(t, ok, "flag 0x%02x should be present", num) {
					assert.Equal(t, wantSet, flag.Set, "flag 0x%02x Set", num)
					assert.NotEmpty(t, flag.Description, "flag 0x%02x Description", num)
				}
			}
		})
	}
}

// TestParseTapeUsage exercises the reposition parse path against real captured
// output of the pinned sg_logs 2.35 (page 0x30, --json). The backhitch fixture is
// a real drive capture with total_suspended_writes set non-zero; the test fails
// if the parser cannot extract that non-zero count. The unsupported fixture is
// valid sg_logs JSON that lacks the Tape usage page, proving "not measured" is
// observable and distinct from a measured zero.
func TestParseTapeUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		fixture      string
		wantCount    int64
		wantMeasured bool
		wantErr      require.ErrorAssertionFunc
	}{
		{
			name:         "clean drive: measured zero",
			fixture:      "testdata/sg_logs_tapeusage_clean.json",
			wantCount:    0,
			wantMeasured: true,
			wantErr:      require.NoError,
		},
		{
			name:         "back-hitched drive: non-zero count",
			fixture:      "testdata/sg_logs_tapeusage_backhitch.json",
			wantCount:    137,
			wantMeasured: true,
			wantErr:      require.NoError,
		},
		{
			name:         "page unsupported: not measured",
			fixture:      "testdata/sg_logs_tapeusage_unsupported.json",
			wantCount:    0,
			wantMeasured: false,
			wantErr:      require.NoError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(tc.fixture)
			require.NoError(t, err, "read fixture")

			count, measured, err := parseTapeUsage(string(data))
			tc.wantErr(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, tc.wantCount, count, "reposition count")
			assert.Equal(t, tc.wantMeasured, measured, "measured")
		})
	}
}

// TestParseTapeUsageMalformed asserts a malformed JSON body is surfaced as an
// error (a page the drive answered but could not be decoded is a real fault), not
// silently reported as a measured zero.
func TestParseTapeUsageMalformed(t *testing.T) {
	t.Parallel()

	count, measured, err := parseTapeUsage("{not json")
	require.Error(t, err)
	assert.Zero(t, count)
	assert.False(t, measured)
}
