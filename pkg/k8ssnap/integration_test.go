//go:build integration

package k8ssnap_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
	"github.com/solidDoWant/tape-archiver/pkg/k8ssnap"
)

// newResolverOrSkip builds a Resolver against the ambient cluster, skipping the
// test when no kubeconfig/in-cluster config is available or the cluster cannot be
// reached (issue #13 AC6).
func newResolverOrSkip(t *testing.T) *k8ssnap.Resolver {
	t.Helper()

	client, err := k8ssnap.NewClient()
	if err != nil {
		t.Skipf("no Kubernetes config available (%s); skipping", err)
	}

	// A cluster-wide list with an impossible selector is a cheap liveness probe:
	// it succeeds (empty) when the API is reachable and errors when it is not.
	if _, err := client.ListVolumeSnapshots(t.Context(), "", "tape-archiver.test/unreachable=1"); err != nil {
		t.Skipf("Kubernetes cluster not reachable (%s); skipping", err)
	}

	return k8ssnap.NewResolver(client, testutil.K8sDatasetParent(t))
}

// TestResolveIntegration resolves a real VolumeSnapshot (named by TAPE_K8S_SNAPSHOT
// in TAPE_K8S_NAMESPACE) to its democratic-csi ZFS snapshot path. It skips when no
// cluster is reachable or no snapshot is configured.
func TestResolveIntegration(t *testing.T) {
	resolver := newResolverOrSkip(t)

	name := testutil.K8sSnapshot(t)
	if name == "" {
		t.Skipf("no VolumeSnapshot configured (%s); skipping", testutil.EnvK8sSnapshot)
	}

	snapshot, err := resolver.Resolve(t.Context(), k8ssnap.Ref{
		Namespace: testutil.K8sNamespace(t),
		Name:      name,
	})
	require.NoError(t, err)

	assert.Equal(t, name, snapshot.VolumeSnapshot)
	assert.NotEmpty(t, snapshot.Dataset, "resolved snapshot should have a dataset path")
	assert.Contains(t, snapshot.SnapshotName, "snapshot-",
		"resolved snapshot name should follow the democratic-csi convention")
}

// TestResolveGroupIntegration resolves all VolumeSnapshots matching
// TAPE_K8S_LABEL_SELECTOR into a group. It skips when no cluster is reachable or no
// selector is configured.
func TestResolveGroupIntegration(t *testing.T) {
	resolver := newResolverOrSkip(t)

	selector := testutil.K8sLabelSelector(t)
	if selector == "" {
		t.Skipf("no label selector configured (%s); skipping", testutil.EnvK8sLabelSelector)
	}

	group, err := resolver.ResolveGroup(t.Context(), k8ssnap.Ref{
		Namespace:     testutil.K8sNamespace(t),
		LabelSelector: selector,
	})
	require.NoError(t, err)
	require.NotEmpty(t, group.Members, "selector should match at least one VolumeSnapshot")

	for _, member := range group.Members {
		assert.NotEmpty(t, member.Dataset)
		assert.Contains(t, member.SnapshotName, "snapshot-")
	}
}
