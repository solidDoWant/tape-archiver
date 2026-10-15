package k8ssnap

import (
	"fmt"
	"regexp"
	"strings"
)

// snapshotNamePattern matches democratic-csi snapshot names ("snapshot-" + UUID);
// anything else is not resolvable (SPEC.md §3, issue #13 AC4).
var snapshotNamePattern = regexp.MustCompile(
	`^snapshot-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// parseHandle splits a zfs-generic snapshotHandle "volume@snapshot" (relative
// to datasetParentName). The detached-snapshot form uses "/" and is rejected.
func parseHandle(handle string) (volume, snapshot string, err error) {
	volume, snapshot, found := strings.Cut(handle, "@")
	if !found {
		return "", "", fmt.Errorf(
			"snapshot handle %q is not an @-snapshot; detached snapshots are unsupported", handle)
	}

	if volume == "" {
		return "", "", fmt.Errorf("snapshot handle %q has an empty volume component", handle)
	}

	if !snapshotNamePattern.MatchString(snapshot) {
		return "", "", fmt.Errorf(
			"snapshot handle %q snapshot component %q does not match snapshot-<uuid>", handle, snapshot)
	}

	return volume, snapshot, nil
}

// absoluteDataset prefixes the handle's volume with datasetParentName (if any).
func absoluteDataset(datasetParent, volume string) string {
	if datasetParent == "" {
		return volume
	}

	return strings.TrimSuffix(datasetParent, "/") + "/" + volume
}
