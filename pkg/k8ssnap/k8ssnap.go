// Package k8ssnap resolves Kubernetes VolumeSnapshots to democratic-csi ZFS
// snapshot paths. Per SPEC.md §16 the control worker resolves (no pool access)
// and the data worker verifies against the pool (no k8s access).
package k8ssnap

import (
	"context"
	"fmt"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
)

// Ref identifies a single snapshot (Name) or a group (LabelSelector; empty
// Namespace means cluster-wide).
type Ref struct {
	Namespace     string
	Name          string
	LabelSelector string
}

// Snapshot is a resolved VolumeSnapshot with its ZFS location and k8s provenance.
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

// Group is a set of snapshots archived as one tar, one subdirectory per volume (SPEC.md §5).
type Group struct {
	Members []Snapshot
}

// SnapshotClient reads VolumeSnapshot and VolumeSnapshotContent objects.
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

// NewResolver returns a Resolver; datasetParent is democratic-csi's
// datasetParentName, which the snapshotHandle omits.
func NewResolver(client SnapshotClient, datasetParent string) *Resolver {
	return &Resolver{client: client, datasetParent: datasetParent}
}

// Resolve resolves one VolumeSnapshot to its ZFS snapshot without touching the
// pool; use Verify on the data side.
func (r *Resolver) Resolve(ctx context.Context, ref Ref) (Snapshot, error) {
	volumeSnapshot, err := r.client.GetVolumeSnapshot(ctx, ref.Namespace, ref.Name)
	if err != nil {
		return Snapshot{}, fmt.Errorf("get VolumeSnapshot %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	return r.resolveSnapshot(ctx, volumeSnapshot)
}

// ResolveGroup resolves every VolumeSnapshot matching ref.LabelSelector. Any
// member failure fails the group, and zero matches is an error so a run never
// silently backs up nothing (SPEC.md §2).
func (r *Resolver) ResolveGroup(ctx context.Context, ref Ref) (Group, error) {
	volumeSnapshots, err := r.client.ListVolumeSnapshots(ctx, ref.Namespace, ref.LabelSelector)
	if err != nil {
		return Group{}, fmt.Errorf("list VolumeSnapshots (namespace %q, selector %q): %w",
			ref.Namespace, ref.LabelSelector, err)
	}

	if len(volumeSnapshots) == 0 {
		return Group{}, fmt.Errorf("label selector %q matched no VolumeSnapshots (namespace %q)",
			ref.LabelSelector, ref.Namespace)
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

// resolveSnapshot requires a ready, bound snapshot and parses its content's handle.
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
