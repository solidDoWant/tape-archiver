package k8ssnap

import (
	"context"
	"fmt"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeSnapshotClient serves canned objects from maps in place of the clientset.
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

// readySnapshot builds a ready, bound VolumeSnapshot and its content carrying handle.
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

	t.Run("empty match errors identifying the selector", func(t *testing.T) {
		t.Parallel()

		resolver := NewResolver(&fakeSnapshotClient{}, testParent)

		_, err := resolver.ResolveGroup(t.Context(), Ref{Namespace: "app", LabelSelector: "app=none"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "app=none")
		assert.Contains(t, err.Error(), "app")
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

// datedSnapshot is a VolumeSnapshot of pvc created hoursAgo before a fixed instant, with its content.
func datedSnapshot(namespace, name, pvc string, hoursAgo int, ready bool) (snapshotv1.VolumeSnapshot, *snapshotv1.VolumeSnapshotContent) {
	handle := fmt.Sprintf("pvc-%s@snapshot-%08d-2349-4611-8b2f-67388602233b", pvc, len(name)*100+hoursAgo)
	snapshot, content := readySnapshot(namespace, name, pvc, "content-"+namespace+"-"+name, handle)
	created := metav1.NewTime(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Add(-time.Duration(hoursAgo) * time.Hour))
	snapshot.Status.CreationTime = &created
	snapshot.Status.ReadyToUse = &ready

	return *snapshot, content
}

func TestResolveGroupNewestPerPVC(t *testing.T) {
	t.Parallel()

	type spec struct {
		namespace, name, pvc string
		hoursAgo             int
		ready                bool
	}

	tests := []struct {
		name        string
		snapshots   []spec
		want        []string
		wantUnready []string
		wantErr     require.ErrorAssertionFunc
		errContains string
	}{
		{
			name: "keeps the newest ready snapshot of each PVC",
			snapshots: []spec{
				{"app", "a-1", "claim-a", 48, true},
				{"app", "a-3", "claim-a", 0, true},
				{"app", "a-2", "claim-a", 24, true},
				{"app", "b-1", "claim-b", 30, true},
				{"app", "b-2", "claim-b", 6, true},
			},
			want:    []string{"app/a-3", "app/b-2"},
			wantErr: require.NoError,
		},
		{
			name: "skips a newer snapshot that is not ready",
			snapshots: []spec{
				{"app", "a-1", "claim-a", 24, true},
				{"app", "a-2", "claim-a", 0, false},
			},
			want:    []string{"app/a-1"},
			wantErr: require.NoError,
		},
		{
			name: "same PVC name in two namespaces is two PVCs",
			snapshots: []spec{
				{"one", "x-1", "data", 5, true},
				{"two", "x-1", "data", 9, true},
				{"two", "x-2", "data", 1, true},
			},
			want:    []string{"one/x-1", "two/x-2"},
			wantErr: require.NoError,
		},
		{
			name: "creation-time tie breaks by greatest name",
			snapshots: []spec{
				{"app", "a-2", "claim-a", 3, true},
				{"app", "a-1", "claim-a", 3, true},
			},
			want:    []string{"app/a-2"},
			wantErr: require.NoError,
		},
		{
			name: "PVC with no ready snapshot is reported and skipped",
			snapshots: []spec{
				{"app", "a-1", "claim-a", 1, true},
				{"app", "b-1", "claim-b", 1, false},
			},
			want:        []string{"app/a-1"},
			wantUnready: []string{"app/claim-b"},
			wantErr:     require.NoError,
		},
		{
			name:        "no ready snapshot at all fails naming the selector",
			snapshots:   []spec{{"app", "a-1", "claim-a", 1, false}},
			wantErr:     require.Error,
			errContains: "app=db",
		},
		{
			name:        "snapshot without a source PVC fails",
			snapshots:   []spec{{"app", "a-1", "", 1, true}},
			wantErr:     require.Error,
			errContains: "app/a-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &fakeSnapshotClient{contents: map[string]*snapshotv1.VolumeSnapshotContent{}}

			for _, snapshot := range tt.snapshots {
				listed, content := datedSnapshot(snapshot.namespace, snapshot.name, snapshot.pvc, snapshot.hoursAgo, snapshot.ready)
				if snapshot.pvc == "" {
					listed.Spec.Source.PersistentVolumeClaimName = nil
				}

				client.listed = append(client.listed, listed)
				client.contents[content.Name] = content
			}

			group, err := NewResolver(client, testParent).ResolveGroup(t.Context(), Ref{LabelSelector: "app=db", NewestPerPVC: true})
			tt.wantErr(t, err)

			if err != nil {
				assert.Contains(t, err.Error(), tt.errContains)

				return
			}

			got := make([]string, 0, len(group.Members))
			for _, member := range group.Members {
				got = append(got, member.Namespace+"/"+member.VolumeSnapshot)
			}

			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantUnready, group.UnreadyPVCs)
		})
	}
}

func TestResolveGroupWithoutNewestPerPVCKeepsAllMatches(t *testing.T) {
	t.Parallel()

	client := &fakeSnapshotClient{contents: map[string]*snapshotv1.VolumeSnapshotContent{}}

	for index, name := range []string{"a-1", "a-2", "a-3"} {
		listed, content := datedSnapshot("app", name, "claim-a", index, true)
		client.listed = append(client.listed, listed)
		client.contents[content.Name] = content
	}

	group, err := NewResolver(client, testParent).ResolveGroup(t.Context(), Ref{LabelSelector: "app=db"})
	require.NoError(t, err)
	assert.Len(t, group.Members, 3)
	assert.Empty(t, group.UnreadyPVCs)
}
