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

	// mount stands in for a dataset mountpoint with snapshot "good" and a
	// non-directory "notadir".
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

func TestParseUserProperties(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want map[string]string
	}{
		{
			name: "mixed native and user properties",
			out: "used\t12345\n" +
				"democratic-csi:managed_resource\ttrue\n" +
				"compression\tlz4\n" +
				"democratic-csi:csi_volume_name\tpvc-0cbef4d8\n",
			want: map[string]string{
				"democratic-csi:managed_resource": "true",
				"democratic-csi:csi_volume_name":  "pvc-0cbef4d8",
			},
		},
		{
			name: "empty user property value is retained",
			out:  "democratic-csi:csi_share_volume_context\t\n",
			want: map[string]string{"democratic-csi:csi_share_volume_context": ""},
		},
		{
			name: "value containing colon is not mistaken for a native property",
			out:  "democratic-csi:server\t10.2.3.1:2049\n",
			want: map[string]string{"democratic-csi:server": "10.2.3.1:2049"},
		},
		{
			name: "no user properties",
			out:  "used\t12345\ncompression\tlz4\n",
			want: map[string]string{},
		},
		{
			name: "empty output",
			out:  "",
			want: map[string]string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := parseUserProperties([]byte(test.out))
			assert.Equal(t, test.want, got)
		})
	}
}

func TestParseHolds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want []string
	}{
		{
			name: "single hold",
			out:  "bulk-pool-01/archive@daily\ttape-archiver-hold-abc123\tTue Jun 28 12:00 2026\n",
			want: []string{"tape-archiver-hold-abc123"},
		},
		{
			name: "multiple holds on one snapshot",
			out: "bulk-pool-01/archive@daily\ttape-archiver-hold-abc123\tTue Jun 28 12:00 2026\n" +
				"bulk-pool-01/archive@daily\tzrepl_replication\tWed Jun 29 09:30 2026\n",
			want: []string{"tape-archiver-hold-abc123", "zrepl_replication"},
		},
		{
			name: "no holds (empty output)",
			out:  "",
			want: nil,
		},
		{
			name: "trailing newline only",
			out:  "\n",
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := parseHolds([]byte(test.out))
			assert.Equal(t, test.want, got)
		})
	}
}

func TestParseMounted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		out       string
		want      bool
		assertErr require.ErrorAssertionFunc
	}{
		{
			name: "mounted",
			out:  "yes\n",
			want: true,
		},
		{
			name: "unmounted",
			out:  "no\n",
			want: false,
		},
		{
			name: "value with surrounding whitespace",
			out:  "  yes  \n",
			want: true,
		},
		{
			name:      "snapshot dash value",
			out:       "-\n",
			assertErr: require.Error,
		},
		{
			name:      "empty output",
			out:       "",
			assertErr: require.Error,
		},
		{
			name:      "garbage value",
			out:       "maybe\n",
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			got, err := parseMounted([]byte(test.out))
			test.assertErr(t, err)
			assert.Equal(t, test.want, got)
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
			name: "dataset name containing a space",
			out:  "tank/media disc\tlogicalreferenced\t123456789\t-\n",
			want: 123456789,
		},
		{
			// Third whitespace token is numeric: strings.Fields would return "2".
			name: "dataset name whose third whitespace token is numeric",
			out:  "tank/media disc 2\tlogicalreferenced\t123456789\t-\n",
			want: 123456789,
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

func TestParseSnapshots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		out       string
		want      []SnapshotInfo
		assertErr require.ErrorAssertionFunc
	}{
		{
			name: "parses name, creation and createtxg",
			out:  "tank/media disc@daily 1\t1700000000\t42\ntank/media disc@daily-2\t1700000100\t50\n",
			want: []SnapshotInfo{
				{Name: "daily 1", Creation: 1700000000, CreateTXG: 42},
				{Name: "daily-2", Creation: 1700000100, CreateTXG: 50},
			},
		},
		{
			name: "drops snapshots of other datasets",
			out:  "tank/media disc/child@x\t1700000000\t42\ntank/media disc@y\t1700000001\t43\n",
			want: []SnapshotInfo{{Name: "y", Creation: 1700000001, CreateTXG: 43}},
		},
		{
			name: "no snapshots",
			out:  "",
		},
		{
			name:      "wrong field count",
			out:       "tank/media disc@y\t1700000001\n",
			assertErr: require.Error,
		},
		{
			name:      "non-numeric creation",
			out:       "tank/media disc@y\tMon Oct 16\t43\n",
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			got, err := parseSnapshots("tank/media disc", []byte(test.out))
			test.assertErr(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}
