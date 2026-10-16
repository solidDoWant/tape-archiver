// Package k8ssnap resolves Kubernetes VolumeSnapshots to democratic-csi ZFS
// snapshot paths. Per SPEC.md §16 the control worker resolves (no pool access)
// and the data worker verifies against the pool (no k8s access).
package k8ssnap

import (
	"context"
	"fmt"
	"slices"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
)

// Ref identifies a single snapshot (Name) or a group (LabelSelector; empty
// Namespace means cluster-wide).
type Ref struct {
	Namespace     string
	Name          string
	LabelSelector string
	// NewestPerPVC keeps only each source PVC's newest ready group match.
	NewestPerPVC bool
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
	// UnreadyPVCs lists "namespace/pvc" matches skipped by NewestPerPVC for having no ready snapshot.
	UnreadyPVCs []string
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

	var group Group

	if ref.NewestPerPVC {
		volumeSnapshots, group.UnreadyPVCs, err = newestPerPVC(volumeSnapshots)
		if err != nil {
			return Group{}, fmt.Errorf("label selector %q (namespace %q): %w", ref.LabelSelector, ref.Namespace, err)
		}

		if len(volumeSnapshots) == 0 {
			return Group{}, fmt.Errorf("label selector %q matched no ready VolumeSnapshots (namespace %q)",
				ref.LabelSelector, ref.Namespace)
		}
	}

	group.Members = make([]Snapshot, 0, len(volumeSnapshots))

	for index := range volumeSnapshots {
		member, err := r.resolveSnapshot(ctx, &volumeSnapshots[index])
		if err != nil {
			return Group{}, err
		}

		group.Members = append(group.Members, member)
	}

	return group, nil
}

// newestPerPVC keeps, per namespace/PVC, the ready snapshot with the newest
// creationTime (ties: greatest name), in list order. PVCs with no ready snapshot
// are returned as unready; a match with no source PVC is an error.
func newestPerPVC(volumeSnapshots []snapshotv1.VolumeSnapshot) ([]snapshotv1.VolumeSnapshot, []string, error) {
	newest := make(map[string]int)

	var order, unready []string

	for index := range volumeSnapshots {
		snapshot := &volumeSnapshots[index]

		pvc := snapshot.Spec.Source.PersistentVolumeClaimName
		if pvc == nil || *pvc == "" {
			return nil, nil, fmt.Errorf("VolumeSnapshot %s/%s has no source PVC to select the newest snapshot by",
				snapshot.Namespace, snapshot.Name)
		}

		key := snapshot.Namespace + "/" + *pvc
		if _, seen := newest[key]; !seen {
			order = append(order, key)
			newest[key] = -1
		}

		status := snapshot.Status
		if status == nil || status.ReadyToUse == nil || !*status.ReadyToUse || status.CreationTime == nil {
			continue
		}

		if current := newest[key]; current < 0 || isNewer(snapshot, &volumeSnapshots[current]) {
			newest[key] = index
		}
	}

	kept := make([]int, 0, len(order))

	for _, key := range order {
		if newest[key] < 0 {
			unready = append(unready, key)

			continue
		}

		kept = append(kept, newest[key])
	}

	slices.Sort(kept)

	selected := make([]snapshotv1.VolumeSnapshot, 0, len(kept))
	for _, index := range kept {
		selected = append(selected, volumeSnapshots[index])
	}

	return selected, unready, nil
}

func isNewer(candidate, current *snapshotv1.VolumeSnapshot) bool {
	candidateTime, currentTime := candidate.Status.CreationTime.Time, current.Status.CreationTime.Time
	if !candidateTime.Equal(currentTime) {
		return candidateTime.After(currentTime)
	}

	return candidate.Name > current.Name
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
