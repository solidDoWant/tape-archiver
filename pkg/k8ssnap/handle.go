package k8ssnap

import (
	"fmt"
	"regexp"
	"strings"
)

// snapshotNamePattern matches democratic-csi snapshot short names: the literal
// "snapshot-" followed by a UUID. democratic-csi names every CSI snapshot this
// way, so a handle whose snapshot component does not match is not a snapshot this
// tool can resolve (SPEC.md §3, issue #13 AC4).
var snapshotNamePattern = regexp.MustCompile(
	`^snapshot-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// parseHandle splits a democratic-csi snapshotHandle into its source volume and
// snapshot short name.
//
// For the zfs-generic drivers the handle is the snapshot path relative to the
// driver's datasetParentName, of the form "<volume>@<snapshot>" — e.g.
// "pvc-<uuid>@snapshot-<uuid>". The volume becomes the leaf of the absolute
// dataset path; the snapshot must match snapshotNamePattern. The detached-snapshot
// form uses a "/" separator instead of "@" and resolves to a full dataset rather
// than an @-snapshot, so it has no "@" and is rejected here.
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

// absoluteDataset joins democratic-csi's datasetParentName with a handle's
// relative volume component to form the absolute ZFS dataset path. An empty
// parent treats the volume as already absolute.
func absoluteDataset(datasetParent, volume string) string {
	if datasetParent == "" {
		return volume
	}

	return strings.TrimSuffix(datasetParent, "/") + "/" + volume
}
