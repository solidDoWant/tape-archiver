package ltfs

import (
	"os"
	"path/filepath"
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

	// These options are non-negotiable (SPEC.md §14).
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

	const (
		uuidA = "5659e353-d2fc-44d4-863c-c814a3d3d10a"
		uuidB = "7a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d"
		// An all-digit final group is still a canonical UUID.
		uuidDigitTail = "12345678-1234-1234-1234-000000000000"
	)

	tests := []struct {
		name         string
		candidates   []indexCandidate
		notBefore    time.Time
		expected     string
		errAssertion require.ErrorAssertionFunc
	}{
		{
			name: "single canonical schema",
			candidates: []indexCandidate{
				{name: uuidA + ".schema", modTime: base},
			},
			expected: uuidA + ".schema",
		},
		{
			name: "canonical preferred over generation-suffixed",
			candidates: []indexCandidate{
				{name: uuidA + "-1.schema", modTime: base.Add(2 * time.Second)},
				{name: uuidA + ".schema", modTime: base},
			},
			expected: uuidA + ".schema",
		},
		{
			name: "non-schema files ignored",
			candidates: []indexCandidate{
				{name: uuidA + ".schema", modTime: base},
				{name: "scratch.tmp", modTime: base.Add(time.Hour)},
			},
			expected: uuidA + ".schema",
		},
		{
			name: "newest canonical when several",
			candidates: []indexCandidate{
				{name: uuidA + ".schema", modTime: base},
				{name: uuidB + ".schema", modTime: base.Add(time.Second)},
			},
			expected: uuidB + ".schema",
		},
		{
			name: "falls back to newest generation-suffixed when no canonical",
			candidates: []indexCandidate{
				{name: uuidA + "-1.schema", modTime: base},
				{name: uuidA + "-2.schema", modTime: base.Add(time.Second)},
			},
			expected: uuidA + "-2.schema",
		},
		{
			// An all-digit final UUID group is canonical, so the fresh capture wins.
			name: "all-digit-tail UUID chosen over older leftover",
			candidates: []indexCandidate{
				{name: uuidA + ".schema", modTime: base},
				{name: uuidDigitTail + ".schema", modTime: base.Add(time.Second)},
			},
			expected: uuidDigitTail + ".schema",
		},
		{
			name: "stale leftover rejected as errStaleIndex",
			candidates: []indexCandidate{
				{name: uuidA + ".schema", modTime: base.Add(-time.Hour)},
			},
			notBefore:    base,
			expected:     "",
			errAssertion: requireErrorIs(errStaleIndex),
		},
		{
			// Stale leftover filtered by mtime; the all-digit-tail fresh capture wins.
			name: "fresh all-digit capture chosen over stale hex leftover",
			candidates: []indexCandidate{
				{name: uuidA + ".schema", modTime: base.Add(-time.Hour)},
				{name: uuidDigitTail + ".schema", modTime: base.Add(time.Second)},
			},
			notBefore: base,
			expected:  uuidDigitTail + ".schema",
		},
		{
			name:         "no schema files",
			candidates:   []indexCandidate{{name: "scratch.tmp", modTime: base}},
			expected:     "",
			errAssertion: requireErrorIs(errNoIndex),
		},
		{
			name:         "empty directory",
			candidates:   nil,
			expected:     "",
			errAssertion: requireErrorIs(errNoIndex),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.errAssertion == nil {
				test.errAssertion = require.NoError
			}

			got, err := pickIndexFile(test.candidates, test.notBefore)
			test.errAssertion(t, err)
			assert.Equal(t, test.expected, got)
		})
	}
}

// requireErrorIs builds an ErrorAssertionFunc asserting the error wraps target.
func requireErrorIs(target error) require.ErrorAssertionFunc {
	return func(t require.TestingT, err error, msgAndArgs ...interface{}) {
		require.ErrorIs(t, err, target, msgAndArgs...)
	}
}

func TestReadIndex(t *testing.T) {
	t.Parallel()

	const validIndex = `<?xml version="1.0" encoding="UTF-8"?>` +
		`<ltfsindex version="2.4.0"><generationnumber>1</generationnumber></ltfsindex>`

	const leftoverName = "5659e353-d2fc-44d4-863c-c814a3d3d10a.schema"

	t.Run("rejects a stale leftover index from a prior format", func(t *testing.T) {
		t.Parallel()

		// Only a stale prior-format capture exists; ReadIndex must not ship it.
		workDir := t.TempDir()
		leftover := filepath.Join(workDir, leftoverName)
		require.NoError(t, os.WriteFile(leftover, []byte(validIndex), 0o644))

		mountStart := time.Now()

		staleTime := mountStart.Add(-time.Hour)
		require.NoError(t, os.Chtimes(leftover, staleTime, staleTime))

		m := &Mount{workDir: workDir, mountStart: mountStart}

		data, err := m.ReadIndex(t.Context())
		require.ErrorIs(t, err, errStaleIndex)
		assert.Nil(t, data)
	})

	t.Run("returns a fresh capture written this cycle", func(t *testing.T) {
		t.Parallel()

		workDir := t.TempDir()
		mountStart := time.Now()

		fresh := filepath.Join(workDir, leftoverName)
		require.NoError(t, os.WriteFile(fresh, []byte(validIndex), 0o644))

		freshTime := mountStart.Add(time.Second)
		require.NoError(t, os.Chtimes(fresh, freshTime, freshTime))

		m := &Mount{workDir: workDir, mountStart: mountStart}

		data, err := m.ReadIndex(t.Context())
		require.NoError(t, err)
		assert.Equal(t, validIndex, string(data))
	})
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
