package backup

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/k8ssnap"
)

// testTapeCapacity is a roomy 2.5 TB; capacity-limit cases set their own.
const testTapeCapacity = 2_500_000_000_000

// fakeResolver returns canned results keyed by ref.
type fakeResolver struct {
	resolve      map[string]k8ssnap.Snapshot
	resolveGroup map[string]k8ssnap.Group
	err          error
}

func (f fakeResolver) Resolve(_ context.Context, ref k8ssnap.Ref) (k8ssnap.Snapshot, error) {
	if f.err != nil {
		return k8ssnap.Snapshot{}, f.err
	}

	return f.resolve[ref.Name], nil
}

func (f fakeResolver) ResolveGroup(_ context.Context, ref k8ssnap.Ref) (k8ssnap.Group, error) {
	if f.err != nil {
		return k8ssnap.Group{}, f.err
	}

	group := f.resolveGroup[ref.LabelSelector]

	// Mirror the real Resolver: an empty match is an error, not an empty group.
	if len(group.Members) == 0 {
		return k8ssnap.Group{}, fmt.Errorf("label selector %q matched no VolumeSnapshots (namespace %q)",
			ref.LabelSelector, ref.Namespace)
	}

	return group, nil
}

// fakePool serves canned properties and sizes; an absent path errors, as zfs
// does for a missing dataset.
type fakePool struct {
	properties map[string]map[string]string
	sizes      map[string]int64
	sizeErr    error
}

func (f fakePool) UserProperties(_ context.Context, dataset string) (map[string]string, error) {
	properties, ok := f.properties[dataset]
	if !ok {
		return nil, errors.New("dataset does not exist")
	}

	return properties, nil
}

// UserProperty mirrors zfs: a missing dataset errors and an unset property is "-".
func (f fakePool) UserProperty(_ context.Context, dataset, property string) (string, error) {
	properties, ok := f.properties[dataset]
	if !ok {
		return "", errors.New("dataset does not exist")
	}

	if value, ok := properties[property]; ok {
		return value, nil
	}

	return "-", nil
}

func (f fakePool) LogicalReferenced(_ context.Context, dataset string) (int64, error) {
	if f.sizeErr != nil {
		return 0, f.sizeErr
	}

	return f.sizes[dataset], nil
}

func boolPtr(value bool) *bool        { return &value }
func floatPtr(value float64) *float64 { return &value }
func k8sSource(name string) config.Source {
	return config.Source{K8s: &config.K8sRef{
		APIVersion: "snapshot.storage.k8s.io/v1",
		Kind:       "VolumeSnapshot",
		Namespace:  "apps",
		Name:       name,
	}}
}

func k8sGroupSource(selector string) config.Source {
	return config.Source{K8s: &config.K8sRef{
		APIVersion:    "snapshot.storage.k8s.io/v1",
		Kind:          "VolumeSnapshot",
		Namespace:     "apps",
		LabelSelector: selector,
	}}
}

func zfsSource(name string) config.Source {
	return config.Source{ZFSPath: &config.ZFSPathSource{Name: name}}
}

// managed is the property map k8ssnap.Verify requires of a democratic-csi snapshot.
var managed = map[string]string{"democratic-csi:managed_resource": "true"}

func TestResolveK8sSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		sources   []config.Source
		resolver  fakeResolver
		assertErr require.ErrorAssertionFunc
		want      []ResolvedArchive
	}{
		{
			name:    "resolves a single snapshot and skips raw sources",
			sources: []config.Source{zfsSource("pool/raw@snap"), k8sSource("db")},
			resolver: fakeResolver{resolve: map[string]k8ssnap.Snapshot{
				"db": {Dataset: "pool/pvc-1", SnapshotName: "snapshot-1", Namespace: "apps", VolumeSnapshot: "db", PVC: "db-pvc"},
			}},
			want: []ResolvedArchive{{
				SourceIndex: 1,
				Label:       "db",
				Compression: true,
				Snapshots: []ResolvedSnapshot{{
					ZFSPath:        "pool/pvc-1@snapshot-1",
					Dataset:        "pool/pvc-1",
					SnapshotName:   "snapshot-1",
					Namespace:      "apps",
					VolumeSnapshot: "db",
					PVC:            "db-pvc",
				}},
			}},
		},
		{
			name:    "resolves a label-selector group into one multi-snapshot archive",
			sources: []config.Source{k8sGroupSource("app=web")},
			resolver: fakeResolver{resolveGroup: map[string]k8ssnap.Group{
				"app=web": {Members: []k8ssnap.Snapshot{
					{Dataset: "pool/pvc-a", SnapshotName: "snapshot-a"},
					{Dataset: "pool/pvc-b", SnapshotName: "snapshot-b"},
				}},
			}},
			want: []ResolvedArchive{{
				SourceIndex: 0,
				Label:       "app-web",
				Compression: true,
				Snapshots: []ResolvedSnapshot{
					{ZFSPath: "pool/pvc-a@snapshot-a", Dataset: "pool/pvc-a", SnapshotName: "snapshot-a"},
					{ZFSPath: "pool/pvc-b@snapshot-b", Dataset: "pool/pvc-b", SnapshotName: "snapshot-b"},
				},
			}},
		},
		{
			name:      "fails when a snapshot cannot be resolved",
			sources:   []config.Source{k8sSource("missing")},
			resolver:  fakeResolver{err: errors.New("VolumeSnapshot not found")},
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			activities := &ResolveControlActivities{
				newResolver: func() (snapshotResolver, error) { return test.resolver, nil },
			}

			got, err := activities.ResolveK8sSources(t.Context(), config.Config{Sources: test.sources})
			test.assertErr(t, err)

			if err == nil {
				assert.Equal(t, test.want, got)
			}
		})
	}
}

type capturingResolver struct {
	gotRef k8ssnap.Ref
}

func (c *capturingResolver) Resolve(_ context.Context, _ k8ssnap.Ref) (k8ssnap.Snapshot, error) {
	return k8ssnap.Snapshot{}, nil
}

func (c *capturingResolver) ResolveGroup(_ context.Context, ref k8ssnap.Ref) (k8ssnap.Group, error) {
	c.gotRef = ref

	return k8ssnap.Group{}, nil
}

// TestResolveK8sSourcesClusterWide (AC1): an omitted namespace passes an empty
// Namespace through to ResolveGroup, resolving cluster-wide (SPEC §5).
func TestResolveK8sSourcesClusterWide(t *testing.T) {
	t.Parallel()

	resolver := &capturingResolver{}
	activities := &ResolveControlActivities{
		newResolver: func() (snapshotResolver, error) { return resolver, nil },
	}

	clusterWide := config.Source{K8s: &config.K8sRef{
		APIVersion:    "snapshot.storage.k8s.io/v1",
		Kind:          "VolumeSnapshot",
		LabelSelector: "backup=nightly",
	}}

	_, err := activities.ResolveK8sSources(t.Context(), config.Config{Sources: []config.Source{clusterWide}})
	require.NoError(t, err)
	assert.Empty(t, resolver.gotRef.Namespace, "empty namespace must pass through to ResolveGroup for cluster-wide resolution")
	assert.Equal(t, "backup=nightly", resolver.gotRef.LabelSelector)
}

// TestResolveK8sSourcesEmptyGroupFails (#132 AC1): an empty-match group fails at
// Resolve, naming the source and its selector.
func TestResolveK8sSourcesEmptyGroupFails(t *testing.T) {
	t.Parallel()

	activities := &ResolveControlActivities{
		newResolver: func() (snapshotResolver, error) { return fakeResolver{}, nil },
	}

	_, err := activities.ResolveK8sSources(t.Context(),
		config.Config{Sources: []config.Source{k8sGroupSource("app=gone")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sources[0]")
	assert.Contains(t, err.Error(), "app=gone")
}

func TestResolveK8sSourcesResolverBuildFailure(t *testing.T) {
	t.Parallel()

	activities := &ResolveControlActivities{
		newResolver: func() (snapshotResolver, error) { return nil, errors.New("no kubeconfig") },
	}

	_, err := activities.ResolveK8sSources(t.Context(), config.Config{Sources: []config.Source{k8sSource("db")}})
	require.ErrorContains(t, err, "build k8s resolver")
}

// TestResolveK8sSourcesRawOnlyNeedsNoResolver: newResolver panics if called.
func TestResolveK8sSourcesRawOnlyNeedsNoResolver(t *testing.T) {
	t.Parallel()

	activities := &ResolveControlActivities{
		newResolver: func() (snapshotResolver, error) {
			t.Fatal("k8s resolver must not be built for a raw-only run")

			return nil, nil
		},
	}

	got, err := activities.ResolveK8sSources(t.Context(), config.Config{Sources: []config.Source{zfsSource("pool/raw@snap")}})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestResolveAndCheck(t *testing.T) {
	t.Parallel()

	fixedRedundancy := config.Redundancy{TargetPercentage: floatPtr(10)}

	tests := []struct {
		name        string
		sources     []config.Source
		k8sArchives []ResolvedArchive
		pool        fakePool
		redundancy  config.Redundancy
		overhead    *float64
		capacity    int64
		assertErr   require.ErrorAssertionFunc
		errContains string
		want        []ResolvedArchive
	}{
		{
			name:    "validates a raw source and estimates its size",
			sources: []config.Source{zfsSource("pool/raw@snap")},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/raw@snap": {}},
				sizes:      map[string]int64{"pool/raw@snap": 1000},
			},
			redundancy: fixedRedundancy,
			capacity:   testTapeCapacity,
			// 1000 * 1.05 * 1.10 = 1155.
			want: []ResolvedArchive{{
				SourceIndex:    0,
				Label:          "raw",
				Compression:    true,
				Snapshots:      []ResolvedSnapshot{{ZFSPath: "pool/raw@snap"}},
				LogicalBytes:   1000,
				EstimatedBytes: 1155,
			}},
		},
		{
			name:       "fails when a raw source does not exist",
			sources:    []config.Source{zfsSource("pool/missing@snap")},
			pool:       fakePool{properties: map[string]map[string]string{}},
			redundancy: fixedRedundancy,
			capacity:   testTapeCapacity,
			assertErr:  require.Error,
		},
		{
			name:        "verifies a managed k8s snapshot and carries it through",
			sources:     []config.Source{k8sSource("db")},
			k8sArchives: []ResolvedArchive{{SourceIndex: 0, Compression: true, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/pvc-1@snapshot-1", Dataset: "pool/pvc-1", SnapshotName: "snapshot-1"}}}},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/pvc-1@snapshot-1": managed},
				sizes:      map[string]int64{"pool/pvc-1@snapshot-1": 2000},
			},
			redundancy: fixedRedundancy,
			capacity:   testTapeCapacity,
			// 2000 * 1.05 * 1.10 = 2310.
			want: []ResolvedArchive{{
				SourceIndex:    0,
				Compression:    true,
				Snapshots:      []ResolvedSnapshot{{ZFSPath: "pool/pvc-1@snapshot-1", Dataset: "pool/pvc-1", SnapshotName: "snapshot-1"}},
				LogicalBytes:   2000,
				EstimatedBytes: 2310,
			}},
		},
		{
			name:        "fails when a k8s snapshot is not democratic-csi managed",
			sources:     []config.Source{k8sSource("db")},
			k8sArchives: []ResolvedArchive{{SourceIndex: 0, Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/pvc-1@snapshot-1", Dataset: "pool/pvc-1", SnapshotName: "snapshot-1"}}}},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/pvc-1@snapshot-1": {}},
				sizes:      map[string]int64{"pool/pvc-1@snapshot-1": 2000},
			},
			redundancy: fixedRedundancy,
			capacity:   testTapeCapacity,
			assertErr:  require.Error,
		},
		{
			name:    "rejects an archive that exceeds one tape's capacity",
			sources: []config.Source{zfsSource("pool/big@snap")},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/big@snap": {}},
				sizes:      map[string]int64{"pool/big@snap": 1000},
			},
			redundancy: fixedRedundancy,
			// Estimate 1155 > capacity 1000.
			capacity:  1000,
			assertErr: require.Error,
		},
		{
			// #224 AC1: an estimate within native but above usable capacity must be
			// rejected at Resolve, not after hours of staging at Pack.
			name:    "rejects an archive that exceeds usable capacity though it fits native capacity",
			sources: []config.Source{zfsSource("pool/band@snap")},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/band@snap": {}},
				sizes:      map[string]int64{"pool/band@snap": 900},
			},
			redundancy: fixedRedundancy,
			overhead:   floatPtr(1.0),
			// Estimate 990; usable 950 (reject) yet 990 <= native 1000.
			capacity:    1000,
			assertErr:   require.Error,
			errContains: "usable capacity",
		},
		{
			// #224 AC2: an estimate within usable capacity still passes.
			name:    "admits an archive whose estimate is within usable capacity",
			sources: []config.Source{zfsSource("pool/fits@snap")},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/fits@snap": {}},
				sizes:      map[string]int64{"pool/fits@snap": 800},
			},
			redundancy: fixedRedundancy,
			overhead:   floatPtr(1.0),
			// Estimate ceil(800 * 1.0 * 1.10) = 881 (float rounding) <= usable 950.
			capacity: 1000,
			want: []ResolvedArchive{{
				SourceIndex:    0,
				Label:          "fits",
				Compression:    true,
				Snapshots:      []ResolvedSnapshot{{ZFSPath: "pool/fits@snap"}},
				LogicalBytes:   800,
				EstimatedBytes: 881,
			}},
		},
		{
			name:    "uses the fill-to-capacity floor for the estimate",
			sources: []config.Source{zfsSource("pool/raw@snap")},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/raw@snap": {}},
				sizes:      map[string]int64{"pool/raw@snap": 1000},
			},
			redundancy: config.Redundancy{FillToCapacity: &config.FillConfig{Floor: 20}},
			capacity:   testTapeCapacity,
			// 1000 * 1.05 * 1.20 = 1260.
			want: []ResolvedArchive{{
				SourceIndex:    0,
				Label:          "raw",
				Compression:    true,
				Snapshots:      []ResolvedSnapshot{{ZFSPath: "pool/raw@snap"}},
				LogicalBytes:   1000,
				EstimatedBytes: 1260,
			}},
		},
		{
			name:    "sums a group's snapshots and honors the compression override",
			sources: []config.Source{{Compression: boolPtr(false), K8s: k8sGroupSource("app=web").K8s}},
			k8sArchives: []ResolvedArchive{{
				SourceIndex: 0,
				Compression: false,
				Snapshots: []ResolvedSnapshot{
					{ZFSPath: "pool/pvc-a@snap", Dataset: "pool/pvc-a", SnapshotName: "snap"},
					{ZFSPath: "pool/pvc-b@snap", Dataset: "pool/pvc-b", SnapshotName: "snap"},
				},
			}},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/pvc-a@snap": managed, "pool/pvc-b@snap": managed},
				sizes:      map[string]int64{"pool/pvc-a@snap": 1000, "pool/pvc-b@snap": 500},
			},
			redundancy: fixedRedundancy,
			capacity:   testTapeCapacity,
			// (1000 + 500) * 1.05 * 1.10 = 1732.5 -> 1733.
			want: []ResolvedArchive{{
				SourceIndex: 0,
				Compression: false,
				Snapshots: []ResolvedSnapshot{
					{ZFSPath: "pool/pvc-a@snap", Dataset: "pool/pvc-a", SnapshotName: "snap"},
					{ZFSPath: "pool/pvc-b@snap", Dataset: "pool/pvc-b", SnapshotName: "snap"},
				},
				LogicalBytes:   1500,
				EstimatedBytes: 1733,
			}},
		},
		{
			name:    "fails when a size read errors",
			sources: []config.Source{zfsSource("pool/raw@snap")},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/raw@snap": {}},
				sizeErr:    errors.New("zfs get failed"),
			},
			redundancy: fixedRedundancy,
			capacity:   testTapeCapacity,
			assertErr:  require.Error,
		},
		{
			// Before #324 a 1 MB operator slice size would have blown the payload bound
			// here; the derived slice size now satisfies it by construction.
			name:    "large source resolves; the derived slice size satisfies the payload bound",
			sources: []config.Source{zfsSource("pool/big@snap")},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/big@snap": {}},
				sizes:      map[string]int64{"pool/big@snap": 500_000_000_000},
			},
			redundancy: fixedRedundancy,
			capacity:   testTapeCapacity,
			// 500e9 * 1.05 * 1.10 = 577_500_000_000.
			want: []ResolvedArchive{{
				SourceIndex:    0,
				Label:          "big",
				Compression:    true,
				Snapshots:      []ResolvedSnapshot{{ZFSPath: "pool/big@snap"}},
				LogicalBytes:   500_000_000_000,
				EstimatedBytes: 577_500_000_000,
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			activities := &ResolveDataActivities{pool: test.pool}
			cfg := config.Config{
				Sources:             test.sources,
				Redundancy:          test.redundancy,
				FeasibilityOverhead: test.overhead,
				Library:             config.Library{TapeCapacityBytes: test.capacity},
			}

			got, err := activities.ResolveAndCheck(t.Context(), ResolveDataInput{Config: cfg, K8sArchives: test.k8sArchives})
			test.assertErr(t, err)

			if test.errContains != "" {
				require.ErrorContains(t, err, test.errContains)
			}

			if err == nil {
				assert.Equal(t, test.want, got)
			}
		})
	}
}

func TestPar2Fraction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		redundancy config.Redundancy
		want       float64
	}{
		{name: "target percentage", redundancy: config.Redundancy{TargetPercentage: floatPtr(15)}, want: 0.15},
		{name: "fill-to-capacity floor", redundancy: config.Redundancy{FillToCapacity: &config.FillConfig{Floor: 5}}, want: 0.05},
		{name: "neither set", redundancy: config.Redundancy{}, want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.InDelta(t, test.want, par2Fraction(test.redundancy), 1e-9)
		})
	}
}

func TestFeasibilityEstimateRoundsUp(t *testing.T) {
	t.Parallel()

	// 1000 * 1.05 * 1.10 = 1155 exactly; 333 * 1.05 * 1.0 = 349.65 -> 350.
	assert.Equal(t, int64(1155), feasibilityEstimate(1000, 1.05, 0.10))
	assert.Equal(t, int64(350), feasibilityEstimate(333, 1.05, 0))
}

func TestCheckPayloadBound(t *testing.T) {
	t.Parallel()

	archive := func(estimatedBytes int64) ResolvedArchive {
		return ResolvedArchive{EstimatedBytes: estimatedBytes}
	}

	tests := []struct {
		name           string
		resolved       []ResolvedArchive
		sliceSizeBytes int64
		assertErr      require.ErrorAssertionFunc
	}{
		{
			// Budget 1 MiB / 256 B = 4096 slices; exactly on the budget passes.
			name:           "estimate exactly on the budget passes",
			resolved:       []ResolvedArchive{archive(1024 * 1024)},
			sliceSizeBytes: 256,
			assertErr:      require.NoError,
		},
		{
			name:           "estimate one slice over the budget is rejected",
			resolved:       []ResolvedArchive{archive(1024*1024 + 1)},
			sliceSizeBytes: 256,
			assertErr:      require.Error,
		},
		{
			name:           "comfortably-sized slice is not rejected",
			resolved:       []ResolvedArchive{archive(3 * 1024 * 1024 * 1024)},
			sliceSizeBytes: 1 << 30,
			assertErr:      require.NoError,
		},
		{
			// AC1: 500 GB at 100 MiB slices is ~4769 slices, over the 4096 budget.
			name:           "tiny slice on a large source is rejected",
			resolved:       []ResolvedArchive{archive(500_000_000_000)},
			sliceSizeBytes: 100 * 1024 * 1024,
			assertErr:      require.Error,
		},
		{
			// The count is summed across archives.
			name:           "slice counts sum across archives",
			resolved:       []ResolvedArchive{archive(600_000), archive(600_000)},
			sliceSizeBytes: 256,
			assertErr:      require.Error,
		},
		{
			// Derived sizes are always >= 1 GiB, so a non-positive size is a derivation bug.
			name:           "non-positive slice size is flagged as an invariant violation",
			resolved:       []ResolvedArchive{archive(1 << 40)},
			sliceSizeBytes: 0,
			assertErr:      require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			test.assertErr(t, checkPayloadBound(test.resolved, test.sliceSizeBytes))
		})
	}
}

// TestDeriveSliceSize (#324): whole GiB, at least 1 GiB, within the payload bound
// and the 1,000-slice per-archive cap, and deterministic.
func TestDeriveSliceSize(t *testing.T) {
	t.Parallel()

	const gib = int64(1) << 30

	archive := func(estimatedBytes int64) ResolvedArchive {
		return ResolvedArchive{EstimatedBytes: estimatedBytes}
	}

	tests := []struct {
		name     string
		resolved []ResolvedArchive
		want     int64
	}{
		{
			name:     "small run floors at 1 GiB",
			resolved: []ResolvedArchive{archive(1024)},
			want:     gib,
		},
		{
			// Both derivation terms are 0.
			name:     "empty work list floors at 1 GiB",
			resolved: nil,
			want:     gib,
		},
		{
			// Whole-run term dominates: 7 TB / 3500 = 2 GB → 2 GiB.
			name: "whole-run term dominates and rounds up to a whole GiB",
			resolved: []ResolvedArchive{
				archive(1_000_000_000_000), archive(1_000_000_000_000), archive(1_000_000_000_000),
				archive(1_000_000_000_000), archive(1_000_000_000_000), archive(1_000_000_000_000),
				archive(1_000_000_000_000),
			},
			want: 2 * gib,
		},
		{
			// Per-archive term: 800 GB / 800 = 1 GB → the 1 GiB floor, ~745 slices.
			name:     "single large archive is bounded by the per-archive term",
			resolved: []ResolvedArchive{archive(800_000_000_000)},
			want:     gib,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := DeriveSliceSize(test.resolved)
			assert.Equal(t, test.want, got, "derived slice size")

			assert.Zero(t, got%gib, "slice size must be a whole number of GiB")
			assert.GreaterOrEqual(t, got, gib, "slice size must be at least 1 GiB")

			assert.Equal(t, got, DeriveSliceSize(test.resolved), "derivation must be deterministic")

			require.NoError(t, checkPayloadBound(test.resolved, got))

			for _, resolvedArchive := range test.resolved {
				sliceCount := ceilDiv(resolvedArchive.EstimatedBytes, got)
				assert.LessOrEqual(t, sliceCount, int64(1000), "no archive may exceed 1,000 slices")
			}
		})
	}
}

// TestResolveFailureAbortsBeforePrepare (AC2/AC3): Prepare is never invoked.
func TestResolveFailureAbortsBeforePrepare(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

	env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
		Return(nil, errors.New("VolumeSnapshot not found"))

	env.OnActivity((&PrepareActivities{}).PrepareArchives, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ PrepareInput) ([]StagedArchive, error) {
			t.Error("Prepare ran despite a Resolve failure")

			return nil, nil
		})

	env.ExecuteWorkflow(Backup, validBackupConfig())

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), "phase Resolve")

	value, err := env.QueryWorkflow(LastCompletedPhaseQuery)
	require.NoError(t, err)

	var lastCompleted string
	require.NoError(t, value.Get(&lastCompleted))
	assert.Empty(t, lastCompleted)
}

var _ snapshotResolver = (*k8ssnap.Resolver)(nil)

// zfsPool also doubles as the k8ssnap.PropertyReader for verification.
var (
	_ poolInspector          = zfsPool{}
	_ k8ssnap.PropertyReader = zfsPool{}
)
