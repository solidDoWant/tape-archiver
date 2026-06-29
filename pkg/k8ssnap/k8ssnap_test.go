package k8ssnap

import (
	"context"
	"fmt"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeSnapshotClient serves prepared VolumeSnapshot/VolumeSnapshotContent objects
// from maps, standing in for the typed clientset so Resolve/ResolveGroup can be
// exercised without a cluster. A missing key yields a not-found-style error.
type fakeSnapshotClient struct {
	snapshots map[string]*snapshotv1.VolumeSnapshot        // keyed by namespace/name
	contents  map[string]*snapshotv1.VolumeSnapshotContent // keyed by name
	listed    []snapshotv1.VolumeSnapshot                  // returned by ListVolumeSnapshots
	listErr   error
}

func (f *fakeSnapshotClient) GetVolumeSnapshot(_ context.Context, namespace, name string) (*snapshotv1.VolumeSnapshot, error) {
	if snapshot, ok := f.snapshots[namespace+"/"+name]; ok {
		return snapshot, nil
	}

	return nil, fmt.Errorf("volumesnapshot %s/%s not found", namespace, name)
}

func (f *fakeSnapshotClient) GetVolumeSnapshotContent(_ context.Context, name string) (*snapshotv1.VolumeSnapshotContent, error) {
	if content, ok := f.contents[name]; ok {
		return content, nil
	}

	return nil, fmt.Errorf("volumesnapshotcontent %s not found", name)
}

func (f *fakeSnapshotClient) ListVolumeSnapshots(_ context.Context, _, _ string) ([]snapshotv1.VolumeSnapshot, error) {
	return f.listed, f.listErr
}

// readySnapshot builds a ready, bound VolumeSnapshot in namespace with the given
// source PVC, plus the matching VolumeSnapshotContent carrying handle.
func readySnapshot(namespace, name, pvc, contentName, handle string) (*snapshotv1.VolumeSnapshot, *snapshotv1.VolumeSnapshotContent) {
	ready := true

	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source: snapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: &pvc},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: &contentName,
			ReadyToUse:                     &ready,
		},
	}

	content := &snapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: contentName},
		Status:     &snapshotv1.VolumeSnapshotContentStatus{SnapshotHandle: &handle},
	}

	return snapshot, content
}

const (
	testParent = "bulk-pool-01/k8s/democratic-csi/nfs/pvcs"
	testHandle = "pvc-0cbef4d8-eaef-4d20-8589-8b3c9dc6b9a3@snapshot-43c0ad84-2349-4611-8b2f-67388602233b"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	t.Run("resolves a bound snapshot to its absolute zfs path", func(t *testing.T) {
		t.Parallel()

		snapshot, content := readySnapshot("media", "db-daily", "db-data", "snapcontent-1", testHandle)
		resolver := NewResolver(&fakeSnapshotClient{
			snapshots: map[string]*snapshotv1.VolumeSnapshot{"media/db-daily": snapshot},
			contents:  map[string]*snapshotv1.VolumeSnapshotContent{"snapcontent-1": content},
		}, testParent)

		got, err := resolver.Resolve(t.Context(), Ref{Namespace: "media", Name: "db-daily"})
		require.NoError(t, err)

		assert.Equal(t, "media", got.Namespace)
		assert.Equal(t, "db-daily", got.VolumeSnapshot)
		assert.Equal(t, "db-data", got.PVC)
		assert.Equal(t, testParent+"/pvc-0cbef4d8-eaef-4d20-8589-8b3c9dc6b9a3", got.Dataset)
		assert.Equal(t, "snapshot-43c0ad84-2349-4611-8b2f-67388602233b", got.SnapshotName)
		assert.Equal(t, testParent+"/pvc-0cbef4d8-eaef-4d20-8589-8b3c9dc6b9a3@snapshot-43c0ad84-2349-4611-8b2f-67388602233b", got.ZFSPath())
	})

	t.Run("missing VolumeSnapshot errors", func(t *testing.T) {
		t.Parallel()

		resolver := NewResolver(&fakeSnapshotClient{}, testParent)

		_, err := resolver.Resolve(t.Context(), Ref{Namespace: "media", Name: "absent"})
		require.Error(t, err)
	})

	t.Run("unbound snapshot errors", func(t *testing.T) {
		t.Parallel()

		snapshot := &snapshotv1.VolumeSnapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "pending"},
			Status:     &snapshotv1.VolumeSnapshotStatus{},
		}
		resolver := NewResolver(&fakeSnapshotClient{
			snapshots: map[string]*snapshotv1.VolumeSnapshot{"media/pending": snapshot},
		}, testParent)

		_, err := resolver.Resolve(t.Context(), Ref{Namespace: "media", Name: "pending"})
		require.Error(t, err)
	})

	t.Run("not-ready snapshot errors", func(t *testing.T) {
		t.Parallel()

		snapshot, content := readySnapshot("media", "db-daily", "db-data", "snapcontent-1", testHandle)
		notReady := false
		snapshot.Status.ReadyToUse = &notReady

		resolver := NewResolver(&fakeSnapshotClient{
			snapshots: map[string]*snapshotv1.VolumeSnapshot{"media/db-daily": snapshot},
			contents:  map[string]*snapshotv1.VolumeSnapshotContent{"snapcontent-1": content},
		}, testParent)

		_, err := resolver.Resolve(t.Context(), Ref{Namespace: "media", Name: "db-daily"})
		require.Error(t, err)
	})

	t.Run("content without snapshotHandle errors", func(t *testing.T) {
		t.Parallel()

		snapshot, content := readySnapshot("media", "db-daily", "db-data", "snapcontent-1", testHandle)
		content.Status.SnapshotHandle = nil

		resolver := NewResolver(&fakeSnapshotClient{
			snapshots: map[string]*snapshotv1.VolumeSnapshot{"media/db-daily": snapshot},
			contents:  map[string]*snapshotv1.VolumeSnapshotContent{"snapcontent-1": content},
		}, testParent)

		_, err := resolver.Resolve(t.Context(), Ref{Namespace: "media", Name: "db-daily"})
		require.Error(t, err)
	})

	t.Run("malformed handle errors", func(t *testing.T) {
		t.Parallel()

		snapshot, content := readySnapshot("media", "db-daily", "db-data", "snapcontent-1", "pvc-0cbef4d8@daily-2026-06-28")
		resolver := NewResolver(&fakeSnapshotClient{
			snapshots: map[string]*snapshotv1.VolumeSnapshot{"media/db-daily": snapshot},
			contents:  map[string]*snapshotv1.VolumeSnapshotContent{"snapcontent-1": content},
		}, testParent)

		_, err := resolver.Resolve(t.Context(), Ref{Namespace: "media", Name: "db-daily"})
		require.Error(t, err)
	})
}

func TestResolveGroup(t *testing.T) {
	t.Parallel()

	t.Run("resolves one member per matched snapshot", func(t *testing.T) {
		t.Parallel()

		handleOne := "pvc-aaaaaaaa-eaef-4d20-8589-8b3c9dc6b9a3@snapshot-11111111-2349-4611-8b2f-67388602233b"
		handleTwo := "pvc-bbbbbbbb-eaef-4d20-8589-8b3c9dc6b9a3@snapshot-22222222-2349-4611-8b2f-67388602233b"
		first, firstContent := readySnapshot("app", "vol-a", "claim-a", "snapcontent-a", handleOne)
		second, secondContent := readySnapshot("app", "vol-b", "claim-b", "snapcontent-b", handleTwo)

		resolver := NewResolver(&fakeSnapshotClient{
			contents: map[string]*snapshotv1.VolumeSnapshotContent{
				"snapcontent-a": firstContent,
				"snapcontent-b": secondContent,
			},
			listed: []snapshotv1.VolumeSnapshot{*first, *second},
		}, testParent)

		group, err := resolver.ResolveGroup(t.Context(), Ref{LabelSelector: "app=db"})
		require.NoError(t, err)
		require.Len(t, group.Members, 2)

		assert.Equal(t, "vol-a", group.Members[0].VolumeSnapshot)
		assert.Equal(t, testParent+"/pvc-aaaaaaaa-eaef-4d20-8589-8b3c9dc6b9a3", group.Members[0].Dataset)
		assert.Equal(t, "vol-b", group.Members[1].VolumeSnapshot)
		assert.Equal(t, "snapshot-22222222-2349-4611-8b2f-67388602233b", group.Members[1].SnapshotName)
	})

	t.Run("empty match yields an empty group", func(t *testing.T) {
		t.Parallel()

		resolver := NewResolver(&fakeSnapshotClient{}, testParent)

		group, err := resolver.ResolveGroup(t.Context(), Ref{LabelSelector: "app=none"})
		require.NoError(t, err)
		assert.Empty(t, group.Members)
	})

	t.Run("one unresolvable member fails the group", func(t *testing.T) {
		t.Parallel()

		good, goodContent := readySnapshot("app", "vol-a", "claim-a", "snapcontent-a", testHandle)
		bad, badContent := readySnapshot("app", "vol-b", "claim-b", "snapcontent-b", "pvc-b/snapshot-bad")

		resolver := NewResolver(&fakeSnapshotClient{
			contents: map[string]*snapshotv1.VolumeSnapshotContent{
				"snapcontent-a": goodContent,
				"snapcontent-b": badContent,
			},
			listed: []snapshotv1.VolumeSnapshot{*good, *bad},
		}, testParent)

		_, err := resolver.ResolveGroup(t.Context(), Ref{LabelSelector: "app=db"})
		require.Error(t, err)
	})
}
