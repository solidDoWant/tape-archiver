package k8ssnap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseHandle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		handle       string
		wantVolume   string
		wantSnapshot string
		assertErr    require.ErrorAssertionFunc
	}{
		{
			name:         "relative zfs-generic handle",
			handle:       "pvc-0cbef4d8-eaef-4d20-8589-8b3c9dc6b9a3@snapshot-43c0ad84-2349-4611-8b2f-67388602233b",
			wantVolume:   "pvc-0cbef4d8-eaef-4d20-8589-8b3c9dc6b9a3",
			wantSnapshot: "snapshot-43c0ad84-2349-4611-8b2f-67388602233b",
		},
		{
			name:         "uppercase uuid is accepted",
			handle:       "pvc-X@snapshot-43C0AD84-2349-4611-8B2F-67388602233B",
			wantVolume:   "pvc-X",
			wantSnapshot: "snapshot-43C0AD84-2349-4611-8B2F-67388602233B",
		},
		{
			name:      "detached snapshot form is rejected",
			handle:    "pvc-0cbef4d8-eaef-4d20-8589-8b3c9dc6b9a3/snapshot-43c0ad84-2349-4611-8b2f-67388602233b",
			assertErr: require.Error,
		},
		{
			name:      "snapshot component not snapshot-uuid",
			handle:    "pvc-0cbef4d8@daily-2026-06-28",
			assertErr: require.Error,
		},
		{
			name:      "snapshot-prefixed but not a uuid",
			handle:    "pvc-0cbef4d8@snapshot-not-a-uuid",
			assertErr: require.Error,
		},
		{
			name:      "empty volume component",
			handle:    "@snapshot-43c0ad84-2349-4611-8b2f-67388602233b",
			assertErr: require.Error,
		},
		{
			name:      "empty handle",
			handle:    "",
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			volume, snapshot, err := parseHandle(test.handle)
			test.assertErr(t, err)

			assert.Equal(t, test.wantVolume, volume)
			assert.Equal(t, test.wantSnapshot, snapshot)
		})
	}
}

func TestAbsoluteDataset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		datasetParent string
		volume        string
		want          string
	}{
		{
			name:          "parent prepended",
			datasetParent: "bulk-pool-01/k8s/democratic-csi/nfs/pvcs",
			volume:        "pvc-0cbef4d8",
			want:          "bulk-pool-01/k8s/democratic-csi/nfs/pvcs/pvc-0cbef4d8",
		},
		{
			name:          "trailing slash on parent is trimmed",
			datasetParent: "bulk-pool-01/k8s/democratic-csi/nfs/pvcs/",
			volume:        "pvc-0cbef4d8",
			want:          "bulk-pool-01/k8s/democratic-csi/nfs/pvcs/pvc-0cbef4d8",
		},
		{
			name:          "empty parent treats volume as absolute",
			datasetParent: "",
			volume:        "bulk-pool-01/archive/pvc-0cbef4d8",
			want:          "bulk-pool-01/archive/pvc-0cbef4d8",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, absoluteDataset(test.datasetParent, test.volume))
		})
	}
}
