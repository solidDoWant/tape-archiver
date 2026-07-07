package backup

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/k8ssnap"
)

// testTapeCapacity is a roomy tape capacity (2.5 TB native) for the size cases
// that should comfortably fit; capacity-limit cases set their own small value.
const testTapeCapacity = 2_500_000_000_000

// fakeResolver is a snapshotResolver returning canned results keyed by the ref it
// is called with, for testing the control activity without a cluster.
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

	return f.resolveGroup[ref.LabelSelector], nil
}

// fakePool is a poolInspector serving canned ZFS properties and sizes keyed by
// dataset/snapshot path, for testing the data activity without a pool. A path
// absent from properties errors, mirroring how zfs exits non-zero for a missing
// dataset.
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

// UserProperty reads one named property, mirroring zfs: a missing dataset errors
// (the existence check ownership verification leans on), and an unset property on
// an existing dataset yields "-".
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

// managed is the user-property map democratic-csi stamps on a managed snapshot,
// which k8ssnap.Verify requires.
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

// capturingResolver records the Ref passed to ResolveGroup so a test can assert
// the resolved reference (notably an empty, cluster-wide Namespace) is plumbed
// through unchanged.
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

// TestResolveK8sSourcesClusterWide asserts a labelSelector source with the namespace
// omitted resolves end to end, passing an empty Namespace through to ResolveGroup so
// resolution spans all namespaces (cluster-wide; SPEC §5). This proves the reconciled
// contract works, not just that the config validates (AC1).
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

// TestResolveK8sSourcesResolverBuildFailure asserts a k8s source whose resolver
// cannot be built fails the run before any data is staged (AC2).
func TestResolveK8sSourcesResolverBuildFailure(t *testing.T) {
	t.Parallel()

	activities := &ResolveControlActivities{
		newResolver: func() (snapshotResolver, error) { return nil, errors.New("no kubeconfig") },
	}

	_, err := activities.ResolveK8sSources(t.Context(), config.Config{Sources: []config.Source{k8sSource("db")}})
	require.ErrorContains(t, err, "build k8s resolver")
}

// TestResolveK8sSourcesRawOnlyNeedsNoResolver asserts a raw-ZFS-only run never
// builds the k8s resolver — newResolver panics if called.
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

	// fixedRedundancy is a 10% target redundancy used across the size cases.
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
			// A 500 GB source fits on one tape (estimate 577.5 GB < 2.5 TB capacity),
			// so the capacity check passes — but a 1 MB slice size yields ~577k
			// slices, whose activity metadata blows past the Temporal payload bound.
			// The run must fail here, in Resolve, before any staging, naming the
			// offending config field.
			name:    "rejects a slice size that would grow the payload past the Temporal limit",
			sources: []config.Source{zfsSource("pool/big@snap")},
			pool: fakePool{
				properties: map[string]map[string]string{"pool/big@snap": {}},
				sizes:      map[string]int64{"pool/big@snap": 500_000_000_000},
			},
			redundancy:  config.Redundancy{TargetPercentage: floatPtr(10), SliceSizeBytes: 1_000_000},
			capacity:    testTapeCapacity,
			assertErr:   require.Error,
			errContains: "redundancy.sliceSizeBytes",
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

	// archive is a ResolvedArchive carrying only the field the bound reads.
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
			// Budget is half of 2 MiB = 1048576 bytes; at 256 B/slice that is 4096
			// slices. A 1 MiB estimate at a 256 B slice size is exactly 4096 slices,
			// landing on the budget — it must pass.
			name:           "estimate exactly on the budget passes",
			resolved:       []ResolvedArchive{archive(1024 * 1024)},
			sliceSizeBytes: 256,
			assertErr:      require.NoError,
		},
		{
			// One byte more of source pushes to 4097 slices, one over the budget.
			name:           "estimate one slice over the budget is rejected",
			resolved:       []ResolvedArchive{archive(1024*1024 + 1)},
			sliceSizeBytes: 256,
			assertErr:      require.Error,
		},
		{
			// A comfortably-sized config (1 GiB slices on a modest source) is far
			// under the budget (AC2).
			name:           "comfortably-sized slice is not rejected",
			resolved:       []ResolvedArchive{archive(3 * 1024 * 1024 * 1024)},
			sliceSizeBytes: 1 << 30,
			assertErr:      require.NoError,
		},
		{
			// A too-small slice on a large source is rejected up front (AC1):
			// 500 GB at 100 MiB slices is ~4769 slices, over the 4096 budget.
			name:           "tiny slice on a large source is rejected",
			resolved:       []ResolvedArchive{archive(500_000_000_000)},
			sliceSizeBytes: 100 * 1024 * 1024,
			assertErr:      require.Error,
		},
		{
			// The whole-run count is summed across archives: two archives that each
			// fit alone can jointly exceed the budget.
			name:           "slice counts sum across archives",
			resolved:       []ResolvedArchive{archive(600_000), archive(600_000)},
			sliceSizeBytes: 256,
			assertErr:      require.Error,
		},
		{
			// A non-positive slice size is guarded upstream by config.Validate; the
			// bound must not divide by zero.
			name:           "non-positive slice size is a no-op",
			resolved:       []ResolvedArchive{archive(1 << 40)},
			sliceSizeBytes: 0,
			assertErr:      require.NoError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			test.assertErr(t, checkPayloadBound(test.resolved, test.sliceSizeBytes))
		})
	}
}

// TestCheckPayloadBoundNamesFieldAndSuggests asserts the rejection message is
// actionable: it names redundancy.sliceSizeBytes and suggests a minimum slice size
// that, when applied, actually clears the bound (AC1).
func TestCheckPayloadBoundNamesFieldAndSuggests(t *testing.T) {
	t.Parallel()

	// A 500 GB source at a 1 MB slice size is ~500k slices, whose metadata far
	// exceeds the budget.
	resolved := []ResolvedArchive{{EstimatedBytes: 500_000_000_000}}
	sliceSizeBytes := int64(1_000_000)

	err := checkPayloadBound(resolved, sliceSizeBytes)
	require.Error(t, err)
	assert.ErrorContains(t, err, "redundancy.sliceSizeBytes")
	assert.ErrorContains(t, err, "increase sliceSizeBytes to at least")

	// The suggested minimum must clear the bound. Recompute it the same way the
	// helper does and confirm the bound passes at that size.
	maxSlices := int64((2 * 1024 * 1024 / 2) / 256)
	headroom := maxSlices - int64(len(resolved))
	suggested := (resolved[0].EstimatedBytes + headroom - 1) / headroom
	require.NoError(t, checkPayloadBound(resolved, suggested))
}

// TestResolveFailureAbortsBeforePrepare asserts that when Resolve fails, the run
// fails before any data is staged: the Prepare activity is never invoked (AC2/AC3).
func TestResolveFailureAbortsBeforePrepare(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)

	env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
		Return(nil, errors.New("VolumeSnapshot not found"))

	// Prepare must never run; if it does, the test fails.
	env.OnActivity((&PrepareActivities{}).PrepareArchives, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ PrepareInput) ([]StagedArchive, error) {
			t.Error("Prepare ran despite a Resolve failure")

			return nil, nil
		})

	env.ExecuteWorkflow(Backup, config.Config{})

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), "phase Resolve")

	value, err := env.QueryWorkflow(LastCompletedPhaseQuery)
	require.NoError(t, err)

	var lastCompleted string
	require.NoError(t, value.Get(&lastCompleted))
	assert.Empty(t, lastCompleted)
}

// confirm *k8ssnap.Resolver satisfies snapshotResolver (compile-time check that
// the production resolver fits the seam the activity depends on).
var _ snapshotResolver = (*k8ssnap.Resolver)(nil)

// confirm zfsPool satisfies poolInspector and that its UserProperties also
// satisfies k8ssnap.PropertyReader, so it doubles as the verification reader.
var (
	_ poolInspector          = zfsPool{}
	_ k8ssnap.PropertyReader = zfsPool{}
)
