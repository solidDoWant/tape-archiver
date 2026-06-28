package ltfs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

func TestMkltfsArgs(t *testing.T) {
	t.Parallel()

	args := mkltfsArgs("/dev/sg0", tape.Barcode("TA0001L6"))

	assert.Equal(t, []string{
		"--device=/dev/sg0",
		"--volume-name=TA0001L6",
		"--force",
	}, args)
}

func TestLtfsArgs(t *testing.T) {
	t.Parallel()

	args := ltfsArgs("/dev/sg0", "/mnt/ltfs", "/var/tmp/ltfs-work")

	// Foreground supervision and the wear-minimising options are non-negotiable
	// (SPEC.md §14); assert each is present and exact.
	assert.Equal(t, []string{
		"-f",
		"/mnt/ltfs",
		"-o", "devname=/dev/sg0",
		"-o", "sync_type=unmount",
		"-o", "capture_index",
		"-o", "work_directory=/var/tmp/ltfs-work",
	}, args)
}

func TestPickIndexFile(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name         string
		candidates   []indexCandidate
		expected     string
		errAssertion require.ErrorAssertionFunc
	}{
		{
			name: "single canonical schema",
			candidates: []indexCandidate{
				{name: "5659e353-d2fc-44d4-863c-c814a3d3d10a.schema", modTime: base},
			},
			expected: "5659e353-d2fc-44d4-863c-c814a3d3d10a.schema",
		},
		{
			name: "canonical preferred over generation-suffixed",
			candidates: []indexCandidate{
				{name: "vol-1.schema", modTime: base.Add(2 * time.Second)},
				{name: "vol.schema", modTime: base},
			},
			expected: "vol.schema",
		},
		{
			name: "non-schema files ignored",
			candidates: []indexCandidate{
				{name: "vol.schema", modTime: base},
				{name: "scratch.tmp", modTime: base.Add(time.Hour)},
			},
			expected: "vol.schema",
		},
		{
			name: "newest canonical when several",
			candidates: []indexCandidate{
				{name: "a.schema", modTime: base},
				{name: "b.schema", modTime: base.Add(time.Second)},
			},
			expected: "b.schema",
		},
		{
			name: "falls back to newest generation-suffixed when no canonical",
			candidates: []indexCandidate{
				{name: "vol-1.schema", modTime: base},
				{name: "vol-2.schema", modTime: base.Add(time.Second)},
			},
			expected: "vol-2.schema",
		},
		{
			name:         "no schema files",
			candidates:   []indexCandidate{{name: "scratch.tmp", modTime: base}},
			expected:     "",
			errAssertion: require.Error,
		},
		{
			name:         "empty directory",
			candidates:   nil,
			expected:     "",
			errAssertion: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.errAssertion == nil {
				test.errAssertion = require.NoError
			}

			got, err := pickIndexFile(test.candidates)
			test.errAssertion(t, err)
			assert.Equal(t, test.expected, got)
		})
	}
}

func TestValidateIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		data         string
		errAssertion require.ErrorAssertionFunc
	}{
		{
			name: "valid ltfs index",
			data: `<?xml version="1.0" encoding="UTF-8"?>` +
				`<ltfsindex version="2.4.0"><generationnumber>1</generationnumber></ltfsindex>`,
		},
		{
			name:         "wrong root element",
			data:         `<notanindex></notanindex>`,
			errAssertion: require.Error,
		},
		{
			name:         "malformed xml",
			data:         `<ltfsindex><unclosed>`,
			errAssertion: require.Error,
		},
		{
			name:         "empty",
			data:         ``,
			errAssertion: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.errAssertion == nil {
				test.errAssertion = require.NoError
			}

			test.errAssertion(t, validateIndex([]byte(test.data)))
		})
	}
}
