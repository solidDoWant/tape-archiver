package k8ssnap

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePropertyReader returns canned user properties (or an error) for any path,
// standing in for pkg/zfs so Verify can be exercised without a pool. It records
// the path it was asked about.
type fakePropertyReader struct {
	properties map[string]string
	err        error
	askedPath  string
}

func (f *fakePropertyReader) UserProperties(_ context.Context, dataset string) (map[string]string, error) {
	f.askedPath = dataset

	if f.err != nil {
		return nil, f.err
	}

	return f.properties, nil
}

func TestVerify(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{
		Dataset:      "bulk-pool-01/k8s/democratic-csi/nfs/pvcs/pvc-0cbef4d8",
		SnapshotName: "snapshot-43c0ad84-2349-4611-8b2f-67388602233b",
	}

	tests := []struct {
		name      string
		reader    *fakePropertyReader
		assertErr require.ErrorAssertionFunc
	}{
		{
			name: "managed snapshot passes",
			reader: &fakePropertyReader{properties: map[string]string{
				"democratic-csi:managed_resource": "true",
				"democratic-csi:csi_volume_name":  "pvc-0cbef4d8",
			}},
		},
		{
			name: "managed_resource not true is rejected",
			reader: &fakePropertyReader{properties: map[string]string{
				"democratic-csi:managed_resource": "false",
			}},
			assertErr: require.Error,
		},
		{
			name:      "managed_resource absent is rejected",
			reader:    &fakePropertyReader{properties: map[string]string{}},
			assertErr: require.Error,
		},
		{
			name:      "missing snapshot (reader error) is rejected",
			reader:    &fakePropertyReader{err: errors.New("dataset does not exist")},
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			err := Verify(t.Context(), test.reader, snapshot)
			test.assertErr(t, err)

			assert.Equal(t, snapshot.ZFSPath(), test.reader.askedPath,
				"Verify should read properties of the @-snapshot path")
		})
	}
}
