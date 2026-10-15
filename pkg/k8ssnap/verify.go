package k8ssnap

import (
	"context"
	"fmt"
)

// managedResourceProperty marks a dataset as democratic-csi-owned. It is the only
// stamped property that verifies ownership; the name properties merely restate
// the path (issue #13, SPEC.md §3).
const managedResourceProperty = "democratic-csi:managed_resource"

// PropertyReader reads one named ZFS user property (satisfied by pkg/zfs). Reading
// by name means a newline in another property's value cannot fabricate it. A
// missing snapshot must yield an error.
type PropertyReader interface {
	UserProperty(ctx context.Context, dataset, property string) (string, error)
}

// Verify confirms the resolved snapshot exists and is democratic-csi-managed
// before any data is staged (SPEC.md §4.3, §16).
func Verify(ctx context.Context, reader PropertyReader, snapshot Snapshot) error {
	path := snapshot.ZFSPath()

	managed, err := reader.UserProperty(ctx, path, managedResourceProperty)
	if err != nil {
		return fmt.Errorf("read user property %s for %s: %w", managedResourceProperty, path, err)
	}

	if managed != "true" {
		return fmt.Errorf("snapshot %s is not democratic-csi managed (%s=%q)",
			path, managedResourceProperty, managed)
	}

	return nil
}
