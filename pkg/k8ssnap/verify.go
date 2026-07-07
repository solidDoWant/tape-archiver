package k8ssnap

import (
	"context"
	"fmt"
)

// managedResourceProperty is the democratic-csi ZFS user property marking a
// dataset or snapshot as provisioned and owned by democratic-csi (value "true").
// Confirmed against the production pool, it is the only stamped property that ties
// a resolved ZFS snapshot back to democratic-csi without merely restating the
// snapshot path; the per-name properties (csi_volume_name, csi_snapshot_name) are
// derived from the same path and so verify nothing. PVC name and namespace are not
// stamped at all. See issue #13 and SPEC.md §3.
const managedResourceProperty = "democratic-csi:managed_resource"

// PropertyReader reads a single named ZFS user property for a dataset or
// snapshot. It is satisfied by pkg/zfs and injected so the data-side check runs
// wherever the pool is reachable (SPEC.md §16). Reading one property by name
// (rather than scraping all of them) means a newline embedded in some other
// property's value can never fabricate the property being checked. A non-existent
// snapshot must yield an error.
type PropertyReader interface {
	UserProperty(ctx context.Context, dataset, property string) (string, error)
}

// Verify is the data-side integrity guard (SPEC.md §4.3 phase 1, §16): before any
// data is staged it confirms the resolved ZFS snapshot exists on the pool and is
// democratic-csi-managed. It returns a non-nil error when the snapshot is absent
// (the reader errors) or its democratic-csi:managed_resource property is not
// "true" — i.e. the resolved path points at something democratic-csi does not own,
// such as a misconfigured dataset parent.
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
