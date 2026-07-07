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

	// Canonical LTFS captures are named after the volume UUID (RFC-4122
	// 8-4-4-4-12 hex); generation copies append "-<n>".
	const (
		uuidA = "5659e353-d2fc-44d4-863c-c814a3d3d10a"
		uuidB = "7a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d"
		// uuidDigitTail's final group is all digits — a valid canonical UUID that
		// the old negative "-<n>.schema" test misclassified as generation-suffixed.
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
			// AC2: an all-digit final UUID group is canonical, so a fresh capture so
			// named is chosen over an older hex-lettered leftover — not misclassified
			// as generation-suffixed and excluded from the canonical tier.
			name: "all-digit-tail UUID chosen over older leftover",
			candidates: []indexCandidate{
				{name: uuidA + ".schema", modTime: base},
				{name: uuidDigitTail + ".schema", modTime: base.Add(time.Second)},
			},
			expected: uuidDigitTail + ".schema",
		},
		{
			// A leftover from a prior format predates the mount; it must be rejected
			// rather than returned as this tape's index.
			name: "stale leftover rejected as errStaleIndex",
			candidates: []indexCandidate{
				{name: uuidA + ".schema", modTime: base.Add(-time.Hour)},
			},
			notBefore:    base,
			expected:     "",
			errAssertion: requireErrorIs(errStaleIndex),
		},
		{
			// AC2 worst case: a stale hex-lettered leftover plus a newer all-digit-tail
			// fresh capture — the stale file is filtered by mtime and the fresh file is
			// recognised as canonical, so the fresh one wins.
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

		// AC1: the work directory (keyed per-barcode, stable across runs) holds only
		// a valid captured index left over from a previous format, and this cycle's
		// capture never appeared. ReadIndex must fail rather than ship the stale map.
		workDir := t.TempDir()
		leftover := filepath.Join(workDir, leftoverName)
		require.NoError(t, os.WriteFile(leftover, []byte(validIndex), 0o644))

		mountStart := time.Now()

		// Back-date the leftover to before the mount started, as a real prior-format
		// capture would be.
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

		// A capture written at unmount postdates the mount start.
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
