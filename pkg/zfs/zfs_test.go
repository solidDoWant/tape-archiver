package zfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotDir(t *testing.T) {
	t.Parallel()

	// mount is a temp dir standing in for a dataset mountpoint, with a single
	// snapshot "good" present under .zfs/snapshot/ and a regular file
	// "notadir" where a snapshot directory would otherwise be.
	mount := t.TempDir()
	snapDir := filepath.Join(mount, ".zfs", "snapshot", "good")
	require.NoError(t, os.MkdirAll(snapDir, 0o755))

	fileSnap := filepath.Join(mount, ".zfs", "snapshot", "notadir")
	require.NoError(t, os.WriteFile(fileSnap, []byte("x"), 0o644))

	tests := []struct {
		name      string
		snapshot  string
		want      string
		assertErr require.ErrorAssertionFunc
	}{
		{
			name:     "existing snapshot",
			snapshot: "good",
			want:     snapDir,
		},
		{
			name:      "missing snapshot",
			snapshot:  "absent",
			assertErr: require.Error,
		},
		{
			name:      "snapshot path is not a directory",
			snapshot:  "notadir",
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			got, err := SnapshotDir(mount, test.snapshot)
			test.assertErr(t, err)

			assert.Equal(t, test.want, got)

			if err == nil {
				assert.True(t, filepath.IsAbs(got), "returned path should be absolute")
			}
		})
	}
}

func TestParseLogicalReferenced(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		out       string
		want      int64
		assertErr require.ErrorAssertionFunc
	}{
		{
			name: "tab-delimited value",
			out:  "bulk-pool-01/archive\tlogicalreferenced\t123456789\t-\n",
			want: 123456789,
		},
		{
			name: "snapshot name with trailing whitespace",
			out:  "  bulk-pool-01/archive@daily\tlogicalreferenced\t0\t-  \n",
			want: 0,
		},
		{
			name:      "empty output",
			out:       "",
			assertErr: require.Error,
		},
		{
			name:      "too few fields",
			out:       "bulk-pool-01\tlogicalreferenced\n",
			assertErr: require.Error,
		},
		{
			name:      "non-numeric value",
			out:       "bulk-pool-01\tlogicalreferenced\t12.3G\t-\n",
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			got, err := parseLogicalReferenced([]byte(test.out))
			test.assertErr(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}
