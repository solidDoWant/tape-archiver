package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPlanBurnSets: copies are chunked into sets of at most len(drives) discs,
// disc j assigned to drives[j].
func TestPlanBurnSets(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		copies int
		drives []string
		want   []burnSet
	}{
		"disabled: zero copies": {
			copies: 0,
			drives: []string{"/dev/sr0"},
			want:   nil,
		},
		"single disc, single drive": {
			copies: 1,
			drives: []string{"/dev/sr0"},
			want:   []burnSet{{{Device: "/dev/sr0", CopyIndex: 0}}},
		},
		"copies == drives: one full set": {
			copies: 2,
			drives: []string{"/dev/sr0", "/dev/sr1"},
			want: []burnSet{{
				{Device: "/dev/sr0", CopyIndex: 0},
				{Device: "/dev/sr1", CopyIndex: 1},
			}},
		},
		"copies < drives: one partial set": {
			copies: 1,
			drives: []string{"/dev/sr0", "/dev/sr1"},
			want:   []burnSet{{{Device: "/dev/sr0", CopyIndex: 0}}},
		},
		"copies > drives: full set then partial": {
			copies: 3,
			drives: []string{"/dev/sr0", "/dev/sr1"},
			want: []burnSet{
				{
					{Device: "/dev/sr0", CopyIndex: 0},
					{Device: "/dev/sr1", CopyIndex: 1},
				},
				{{Device: "/dev/sr0", CopyIndex: 2}},
			},
		},
		"copies a multiple of drives: two full sets": {
			copies: 4,
			drives: []string{"/dev/sr0", "/dev/sr1"},
			want: []burnSet{
				{
					{Device: "/dev/sr0", CopyIndex: 0},
					{Device: "/dev/sr1", CopyIndex: 1},
				},
				{
					{Device: "/dev/sr0", CopyIndex: 2},
					{Device: "/dev/sr1", CopyIndex: 3},
				},
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := planBurnSets(test.copies, test.drives)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

// TestPlanBurnSetsErrors covers the defensive guards against a misconfiguration.
func TestPlanBurnSetsErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		copies  int
		drives  []string
		wantErr string
	}{
		"negative copies":             {copies: -1, drives: []string{"/dev/sr0"}, wantErr: "must not be negative"},
		"copies but no burners":       {copies: 2, drives: nil, wantErr: "no optical burners configured"},
		"copies but empty burner set": {copies: 1, drives: []string{}, wantErr: "no optical burners configured"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := planBurnSets(test.copies, test.drives)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErr)
			assert.Nil(t, got)
		})
	}
}
