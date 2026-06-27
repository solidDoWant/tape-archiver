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
			fixture:    "testdata/sg_logs_tapealert_clean.txt",
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
			fixture:    "testdata/sg_logs_tapealert_flagged.txt",
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

func TestParseRepositions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// input is the sg_logs output to parse.
		input string
		want  int64
	}{
		{
			name:  "no repositions field",
			input: "Sequential-Access Device page  [0x24]:\n  Thread count = 0\n",
			want:  0,
		},
		{
			name:  "repositions field present",
			input: "Sequential-Access Device page  [0x24]:\n  Repositions = 42\n",
			want:  42,
		},
		{
			name:  "back-hitch spelling variant",
			input: "Back-hitches = 7\n",
			want:  7,
		},
		{
			name:  "empty output",
			input: "",
			want:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, parseRepositions(tc.input))
		})
	}
}
