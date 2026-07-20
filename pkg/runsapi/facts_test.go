package runsapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

func TestHumanizeBytes(t *testing.T) {
	tests := []struct {
		name  string
		bytes int64
		want  string
	}{
		{name: "zero", bytes: 0, want: "0 B"},
		{name: "sub-kilobyte stays in bytes", bytes: 512, want: "512 B"},
		{name: "kilobyte boundary", bytes: 1000, want: "1.0 kB"},
		{name: "kilobytes", bytes: 2400, want: "2.4 kB"},
		{name: "megabytes", bytes: 5_600_000, want: "5.6 MB"},
		{name: "gigabytes", bytes: 6_000_000_000, want: "6.0 GB"},
		{name: "terabytes", bytes: 18_000_000_000_000, want: "18.0 TB"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, humanizeBytes(test.bytes))
		})
	}
}

func TestGroupDigits(t *testing.T) {
	tests := []struct {
		name string
		n    int64
		want string
	}{
		{name: "single digit", n: 5, want: "5"},
		{name: "exactly three digits", n: 512, want: "512"},
		{name: "four digits", n: 2400, want: "2,400"},
		{name: "billions", n: 6_000_000_000, want: "6,000,000,000"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, groupDigits(test.n))
		})
	}
}

// TestPrepareFactsStagedBytes confirms the Prepare phase's stagedBytes fact
// sums every staged archive, displays it humanized, and carries the exact
// byte count in Title for the web UI's hover text.
func TestPrepareFactsStagedBytes(t *testing.T) {
	records := []activityRecord{{
		Name:      "PrepareArchives",
		Completed: true,
		Result: mustEncode(t, []backup.StagedArchive{
			{SourceIndex: 0, SizeBytes: 4_000_000_000},
			{SourceIndex: 1, SizeBytes: 2_000_000_000},
		}),
	}}

	facts := prepareFacts(records)

	byKey := make(map[string]PhaseFact, len(facts))
	for _, fact := range facts {
		byKey[fact.Key] = fact
	}

	staged, ok := byKey["stagedBytes"]
	require.True(t, ok, "stagedBytes fact present")
	assert.Equal(t, "Staged bytes", staged.Label)
	assert.Equal(t, "6.0 GB", staged.Value)
	assert.Equal(t, "6,000,000,000 bytes", staged.Title)
}
