// Package k8ssnap resolves Kubernetes VolumeSnapshot and snapshot-group
// references to democratic-csi ZFS snapshot paths, and guards their referential
// integrity against the pool before any data is staged.
//
// Resolution follows the SPEC.md §16 ownership split. The control worker calls
// Resolve/ResolveGroup, which read VolumeSnapshot objects from the
// snapshot.storage.k8s.io API group, follow each to its bound
// VolumeSnapshotContent, and turn the CSI snapshotHandle into an absolute ZFS
// snapshot path. The data worker — where the pool is reachable — then calls Verify
// to confirm the resolved snapshot exists and is democratic-csi-managed. No pool
// access happens during resolution; no k8s access happens during verification.
package k8ssnap

import (
	"context"
	"fmt"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
)

// Ref identifies the VolumeSnapshot(s) to resolve. Exactly one of Name or
// LabelSelector is used: Name targets a single snapshot in Namespace;
// LabelSelector selects a group, with an empty Namespace meaning cluster-wide. It
// mirrors the run-config K8sRef without coupling this package to internal/config.
type Ref struct {
	Namespace     string
	Name          string
	LabelSelector string
}

// Snapshot is a resolved VolumeSnapshot mapped to its democratic-csi ZFS location
// and k8s provenance. Dataset is the absolute ZFS dataset path and SnapshotName the
// ZFS snapshot short name; together (via ZFSPath) they form the @-snapshot tar
// source.
type Snapshot struct {
	Namespace      string // namespace of the source VolumeSnapshot
	VolumeSnapshot string // VolumeSnapshot object name
	PVC            string // source PersistentVolumeClaim name, when bound from one
	Dataset        string // absolute ZFS dataset path, e.g. bulk-pool-01/.../pvc-<uuid>
	SnapshotName   string // ZFS snapshot short name, e.g. snapshot-<uuid>
	Handle         string // raw CSI snapshotHandle from the bound VolumeSnapshotContent
}

// ZFSPath returns the absolute ZFS snapshot path "<Dataset>@<SnapshotName>".
func (s Snapshot) ZFSPath() string {
	return s.Dataset + "@" + s.SnapshotName
}

// Group is a set of resolved snapshots archived together as a single tar, one
// subdirectory per member volume (SPEC.md §5).
type Group struct {
	Members []Snapshot
}

// SnapshotClient reads VolumeSnapshot and VolumeSnapshotContent objects from the
// snapshot.storage.k8s.io API group. It is the seam between this package and the
// typed external-snapshotter clientset, letting resolution be unit-tested without
// a live cluster.
type SnapshotClient interface {
	GetVolumeSnapshot(ctx context.Context, namespace, name string) (*snapshotv1.VolumeSnapshot, error)
	GetVolumeSnapshotContent(ctx context.Context, name string) (*snapshotv1.VolumeSnapshotContent, error)
	ListVolumeSnapshots(ctx context.Context, namespace, labelSelector string) ([]snapshotv1.VolumeSnapshot, error)
}

// Resolver maps VolumeSnapshot references to democratic-csi ZFS snapshot paths.
type Resolver struct {
	client        SnapshotClient
	datasetParent string
}

// NewResolver returns a Resolver reading snapshots through client and resolving
// handles against datasetParent — democratic-csi's datasetParentName (e.g.
// bulk-pool-01/k8s/democratic-csi/nfs/pvcs), which is stripped from the
// snapshotHandle and so must be prepended to rebuild the absolute path. An empty
// datasetParent treats the handle's volume component as already absolute.
func NewResolver(client SnapshotClient, datasetParent string) *Resolver {
	return &Resolver{client: client, datasetParent: datasetParent}
}

// Resolve resolves a single VolumeSnapshot (ref.Namespace + ref.Name) to its
// democratic-csi ZFS snapshot. It reads the VolumeSnapshot, follows its bound
// VolumeSnapshotContent to the CSI snapshotHandle, and parses that into the ZFS
// dataset and snapshot name (SPEC.md §4.3 phase 1). It does not touch the pool —
// use Verify on the data side to confirm the snapshot exists and is managed.
func (r *Resolver) Resolve(ctx context.Context, ref Ref) (Snapshot, error) {
	volumeSnapshot, err := r.client.GetVolumeSnapshot(ctx, ref.Namespace, ref.Name)
	if err != nil {
		return Snapshot{}, fmt.Errorf("get VolumeSnapshot %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	return r.resolveSnapshot(ctx, volumeSnapshot)
}

// ResolveGroup resolves every VolumeSnapshot matching ref.LabelSelector into a
// Group with one member per snapshot. An empty ref.Namespace selects across all
// namespaces (SPEC.md §5; issue #13 AC2). Each match must resolve, so a single
// failure fails the group.
func (r *Resolver) ResolveGroup(ctx context.Context, ref Ref) (Group, error) {
	volumeSnapshots, err := r.client.ListVolumeSnapshots(ctx, ref.Namespace, ref.LabelSelector)
	if err != nil {
		return Group{}, fmt.Errorf("list VolumeSnapshots (namespace %q, selector %q): %w",
			ref.Namespace, ref.LabelSelector, err)
	}

	group := Group{Members: make([]Snapshot, 0, len(volumeSnapshots))}

	for index := range volumeSnapshots {
		member, err := r.resolveSnapshot(ctx, &volumeSnapshots[index])
		if err != nil {
			return Group{}, err
		}

		group.Members = append(group.Members, member)
	}

	return group, nil
}

// resolveSnapshot turns a fetched VolumeSnapshot into a resolved Snapshot. It
// requires the snapshot to be ready and bound, follows the binding to read the
// CSI snapshotHandle, and parses that handle into the absolute ZFS path.
func (r *Resolver) resolveSnapshot(ctx context.Context, volumeSnapshot *snapshotv1.VolumeSnapshot) (Snapshot, error) {
	id := volumeSnapshot.Namespace + "/" + volumeSnapshot.Name

	status := volumeSnapshot.Status
	if status == nil || status.BoundVolumeSnapshotContentName == nil || *status.BoundVolumeSnapshotContentName == "" {
		return Snapshot{}, fmt.Errorf("VolumeSnapshot %s has no bound VolumeSnapshotContent", id)
	}

	if status.ReadyToUse == nil || !*status.ReadyToUse {
		return Snapshot{}, fmt.Errorf("VolumeSnapshot %s is not ready to use", id)
	}

	contentName := *status.BoundVolumeSnapshotContentName

	content, err := r.client.GetVolumeSnapshotContent(ctx, contentName)
	if err != nil {
		return Snapshot{}, fmt.Errorf("get VolumeSnapshotContent %s for %s: %w", contentName, id, err)
	}

	if content.Status == nil || content.Status.SnapshotHandle == nil || *content.Status.SnapshotHandle == "" {
		return Snapshot{}, fmt.Errorf("VolumeSnapshotContent %s for %s has no snapshotHandle", contentName, id)
	}

	handle := *content.Status.SnapshotHandle

	volume, snapshotName, err := parseHandle(handle)
	if err != nil {
		return Snapshot{}, fmt.Errorf("VolumeSnapshot %s: %w", id, err)
	}

	resolved := Snapshot{
		Namespace:      volumeSnapshot.Namespace,
		VolumeSnapshot: volumeSnapshot.Name,
		Dataset:        absoluteDataset(r.datasetParent, volume),
		SnapshotName:   snapshotName,
		Handle:         handle,
	}

	if source := volumeSnapshot.Spec.Source.PersistentVolumeClaimName; source != nil {
		resolved.PVC = *source
	}

	return resolved, nil
}
