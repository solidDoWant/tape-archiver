package k8ssnap

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePropertyReader returns a canned single-property value (or an error) for any
// path, standing in for pkg/zfs so Verify can be exercised without a pool. It
// records the path and property name it was asked about. properties maps a
// property name to its value; an unread property yields "-" as real zfs does.
type fakePropertyReader struct {
	properties    map[string]string
	err           error
	askedPath     string
	askedProperty string
}

func (f *fakePropertyReader) UserProperty(_ context.Context, dataset, property string) (string, error) {
	f.askedPath = dataset
	f.askedProperty = property

	if f.err != nil {
		return "", f.err
	}

	if value, ok := f.properties[property]; ok {
		return value, nil
	}

	return "-", nil
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
			assert.Equal(t, managedResourceProperty, test.reader.askedProperty,
				"Verify should read the managed_resource property by name, not scrape all properties")
		})
	}
}
