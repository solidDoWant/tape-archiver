package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/k8ssnap"
)

// fakeClaims serves canned claim listings and resolutions.
type fakeClaims struct {
	listed  map[string][]k8ssnap.ClaimRef // keyed by "namespace|selector"
	claims  map[k8ssnap.ClaimRef]k8ssnap.Claim
	errs    map[k8ssnap.ClaimRef]error
	listErr error
}

func (f *fakeClaims) ListClaims(_ context.Context, namespace, labelSelector string) ([]k8ssnap.ClaimRef, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}

	if refs := f.listed[namespace+"|"+labelSelector]; len(refs) > 0 {
		return refs, nil
	}

	return nil, fmt.Errorf("label selector %q: %w", labelSelector, k8ssnap.ErrNoClaims)
}

func (f *fakeClaims) ResolveClaim(_ context.Context, ref k8ssnap.ClaimRef) (k8ssnap.Claim, error) {
	if err := f.errs[ref]; err != nil {
		return k8ssnap.Claim{}, err
	}

	return f.claims[ref], nil
}

func claimControl(claims *fakeClaims) *ResolveControlActivities {
	return &ResolveControlActivities{newClaimResolver: func() (claimResolver, error) { return claims, nil }}
}

func pvcSource(namespace, name string) config.Source {
	return config.Source{K8s: &config.K8sRef{APIVersion: "v1", Kind: config.KindPVC, Namespace: namespace, Name: name}}
}

func pvcSelectorSource(namespace, selector string) config.Source {
	return config.Source{K8s: &config.K8sRef{APIVersion: "v1", Kind: config.KindPVC, Namespace: namespace, LabelSelector: selector}}
}

func isNonRetryable(err error) bool {
	var applicationErr *temporal.ApplicationError

	return errors.As(err, &applicationErr) && applicationErr.NonRetryable()
}

// TestListClaimSources: each PVC selector source lists its claims; a selector
// matching nothing fails non-retryably naming the source.
func TestListClaimSources(t *testing.T) {
	t.Parallel()

	claims := &fakeClaims{listed: map[string][]k8ssnap.ClaimRef{
		"|tape=true": {{Namespace: "apps", Name: "db"}, {Namespace: "media", Name: "library"}},
	}}

	cfg := config.Config{Sources: []config.Source{
		pvcSource("apps", "named"),
		zfsSource("pool/raw"),
		pvcSelectorSource("", "tape=true"),
	}}

	matches, err := claimControl(claims).ListClaimSources(t.Context(), cfg)
	require.NoError(t, err)
	assert.Equal(t, []ClaimMatches{{SourceIndex: 2, Claims: []k8ssnap.ClaimRef{
		{Namespace: "apps", Name: "db"}, {Namespace: "media", Name: "library"},
	}}}, matches)

	cfg.Sources[2].K8s.LabelSelector = "tape=nope"
	_, err = claimControl(claims).ListClaimSources(t.Context(), cfg)
	require.ErrorContains(t, err, "sources[2] PersistentVolumeClaim")
	assert.True(t, isNonRetryable(err), "no match cannot be fixed by retrying")

	_, err = claimControl(&fakeClaims{listErr: errors.New("apiserver unavailable")}).ListClaimSources(t.Context(), cfg)
	require.Error(t, err)
	assert.False(t, isNonRetryable(err), "an API failure is retried")
}

// TestResolveClaimSources: a named claim resolves to its live dataset with its
// provenance; an unsupported claim fails non-retryably naming it.
func TestResolveClaimSources(t *testing.T) {
	t.Parallel()

	library := k8ssnap.ClaimRef{Namespace: "media", Name: "library"}
	pending := k8ssnap.ClaimRef{Namespace: "media", Name: "pending"}
	flaky := k8ssnap.ClaimRef{Namespace: "media", Name: "flaky"}

	claims := &fakeClaims{
		claims: map[k8ssnap.ClaimRef]k8ssnap.Claim{
			library: {Namespace: "media", Name: "library", Volume: "pvc-1", Dataset: "pool/k8s/pvc-1"},
		},
		errs: map[k8ssnap.ClaimRef]error{
			pending: fmt.Errorf("PersistentVolumeClaim media/pending is not bound: %w", k8ssnap.ErrUnsupportedClaim),
			flaky:   errors.New("get PersistentVolumeClaim media/flaky: timeout"),
		},
	}

	tests := []struct {
		name         string
		source       config.Source
		want         []ResolvedArchive
		assertErr    require.ErrorAssertionFunc
		nonRetryable bool
	}{
		{
			name:   "bound claim",
			source: pvcSource("media", "library"),
			want: []ResolvedArchive{{
				SourceIndex: 0, Label: "library", Compression: true,
				Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/k8s/pvc-1", Namespace: "media", PVC: "library"}},
			}},
		},
		{name: "unsupported claim", source: pvcSource("media", "pending"), assertErr: require.Error, nonRetryable: true},
		{name: "transient API failure", source: pvcSource("media", "flaky"), assertErr: require.Error},
		{name: "unexpanded selector", source: pvcSelectorSource("", "tape=true"), assertErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			got, err := claimControl(claims).ResolveK8sSources(t.Context(), config.Config{Sources: []config.Source{test.source}})
			test.assertErr(t, err)
			assert.Equal(t, test.want, got)

			if err != nil {
				assert.ErrorContains(t, err, "sources[0]")
				assert.Equal(t, test.nonRetryable, isNonRetryable(err))
			}
		})
	}
}

func TestExpandClaimSources(t *testing.T) {
	t.Parallel()

	off := boolPtr(false)
	long := strings.Repeat("a", 45)

	tests := []struct {
		name        string
		sources     []config.Source
		matches     []ClaimMatches
		want        []config.Source
		errContains string
	}{
		{
			name:    "selector expands to named claims keeping compression",
			sources: []config.Source{zfsSource("pool/x"), {K8s: pvcSelectorSource("", "tape=true").K8s, Compression: off}},
			matches: []ClaimMatches{{SourceIndex: 1, Claims: []k8ssnap.ClaimRef{{Namespace: "apps", Name: "db"}, {Namespace: "media", Name: "library"}}}},
			want: []config.Source{
				zfsSource("pool/x"),
				{K8s: pvcSource("apps", "db").K8s, Compression: off},
				{K8s: pvcSource("media", "library").K8s, Compression: off},
			},
		},
		{
			name:    "same name in two namespaces gets namespace-qualified labels",
			sources: []config.Source{pvcSource("apps", "data"), pvcSelectorSource("media", "tape=true")},
			matches: []ClaimMatches{{SourceIndex: 1, Claims: []k8ssnap.ClaimRef{{Namespace: "media", Name: "data"}}}},
			want: []config.Source{
				{K8s: pvcSource("apps", "data").K8s, Label: ptrString("apps-data")},
				{K8s: pvcSource("media", "data").K8s, Label: ptrString("media-data")},
			},
		},
		{
			name: "an operator label is kept",
			sources: []config.Source{
				{K8s: pvcSource("apps", "data").K8s, Label: ptrString("mine")},
				pvcSource("media", "data"),
			},
			want: []config.Source{
				{K8s: pvcSource("apps", "data").K8s, Label: ptrString("mine")},
				{K8s: pvcSource("media", "data").K8s, Label: ptrString("media-data")},
			},
		},
		{
			name:        "labels still colliding after truncation fail",
			sources:     []config.Source{pvcSource(long+"1", "data"), pvcSource(long+"2", "data")},
			matches:     nil,
			errContains: "would both be archived as",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := expandClaimSources(test.sources, test.matches)
			if test.errContains != "" {
				require.ErrorContains(t, err, test.errContains)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, test.want, got)
			assert.NotEqual(t, sourceLabel(got[len(got)-1]), sourceLabel(got[len(got)-2]), "archives are told apart")
		})
	}
}

func ptrString(value string) *string { return &value }

// TestResolveAndCheckVerifiesClaimDataset: the data worker re-checks that a
// claim's dataset is democratic-csi-managed before it is snapshotted.
func TestResolveAndCheckVerifiesClaimDataset(t *testing.T) {
	t.Parallel()

	archive := ResolvedArchive{SourceIndex: 0, Label: "library", Compression: true,
		Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/k8s/pvc-1", Namespace: "media", PVC: "library"}}}
	cfg := config.Config{
		Sources:    []config.Source{pvcSource("media", "library")},
		Library:    config.Library{TapeCapacityBytes: testTapeCapacity},
		Redundancy: config.Redundancy{TargetPercentage: floatPtr(10)},
	}

	tests := []struct {
		name      string
		managed   string
		assertErr require.ErrorAssertionFunc
	}{
		{name: "managed", managed: "true", assertErr: require.NoError},
		{name: "not democratic-csi managed", managed: "false", assertErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pool := fakePool{
				properties: map[string]map[string]string{"pool/k8s/pvc-1": {"democratic-csi:managed_resource": test.managed}},
				sizes:      map[string]int64{"pool/k8s/pvc-1": 1000},
			}

			got, err := (&ResolveDataActivities{pool: pool}).ResolveAndCheck(t.Context(),
				ResolveDataInput{Config: cfg, K8sArchives: []ResolvedArchive{archive}})
			test.assertErr(t, err)

			if err != nil {
				assert.ErrorContains(t, err, "media/library")

				return
			}

			require.Len(t, got, 1)
			assert.Equal(t, archive.Snapshots, got[0].Snapshots)
		})
	}
}

// TestBackupClaimSelectorSource: a PVC selector expands to one archive per
// claim, each staged from a run-scoped snapshot that keeps the claim's
// provenance for the report.
func TestBackupClaimSelectorSource(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	expectReportDeliverSuccess(env)

	env.OnActivity((&ResolveControlActivities{}).ListClaimSources, mock.Anything, mock.Anything).
		Return([]ClaimMatches{{SourceIndex: 0, Claims: []k8ssnap.ClaimRef{{Namespace: "apps", Name: "data"}, {Namespace: "media", Name: "data"}}}}, nil)

	var k8sInput config.Config

	env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
		Return(func(_ context.Context, cfg config.Config) ([]ResolvedArchive, error) {
			k8sInput = cfg

			return []ResolvedArchive{
				{SourceIndex: 0, Label: "apps-data", Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/pvc-a", Namespace: "apps", PVC: "data"}}},
				{SourceIndex: 1, Label: "media-data", Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/pvc-b", Namespace: "media", PVC: "data"}}},
			}, nil
		})
	env.OnActivity((&ResolveDataActivities{}).ResolveAndCheck, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input ResolveDataInput) ([]ResolvedArchive, error) {
			return input.K8sArchives, nil
		})

	var createInput RunSnapshotInput

	env.OnActivity((&RunSnapshotActivities{}).CreateRunSnapshots, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input RunSnapshotInput) ([]CreatedSnapshot, error) {
			createInput = input

			return []CreatedSnapshot{{ZFSPath: input.Snapshots[0]}, {ZFSPath: input.Snapshots[1]}}, nil
		})
	env.OnActivity((&HoldActivities{}).HoldSnapshots, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&HoldActivities{}).ReleaseSnapshots, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&RunSnapshotActivities{}).DestroyRunSnapshots, mock.Anything, mock.Anything).Return(nil)

	var prepareInput PrepareInput

	env.OnActivity((&PrepareActivities{}).PrepareArchives, mock.Anything, mock.Anything).
		Return(func(_ context.Context, input PrepareInput) ([]StagedArchive, error) {
			prepareInput = input

			return nil, nil
		})

	cfg := validBackupConfig()
	cfg.Sources = []config.Source{pvcSelectorSource("", "tape=true")}

	env.ExecuteWorkflow(Backup, cfg)
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	assert.Equal(t, []config.Source{
		{K8s: pvcSource("apps", "data").K8s, Label: ptrString("apps-data")},
		{K8s: pvcSource("media", "data").K8s, Label: ptrString("media-data")},
	}, k8sInput.Sources, "one named source per claim, told apart by namespace")

	name := RunSnapshotName(testRunID)
	assert.Equal(t, []string{"pool/pvc-a@" + name, "pool/pvc-b@" + name}, createInput.Snapshots)

	require.Len(t, prepareInput.Archives, 2)

	staged := prepareInput.Archives[1].Snapshots[0]
	assert.Equal(t, "pool/pvc-b@"+name, staged.ZFSPath)
	assert.True(t, staged.RunScoped)
	assert.Equal(t, "media", staged.Namespace)
	assert.Equal(t, "data", staged.PVC)
}
