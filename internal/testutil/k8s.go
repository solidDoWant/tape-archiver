package testutil

import (
	"os"
	"testing"
)

const (
	// EnvK8sNamespace names the namespace of a VolumeSnapshot the integration
	// tests resolve. Empty when unset.
	EnvK8sNamespace = "TAPE_K8S_NAMESPACE"
	// EnvK8sSnapshot names a VolumeSnapshot known to exist (in EnvK8sNamespace)
	// for exercising single-snapshot resolution. Empty when unset.
	EnvK8sSnapshot = "TAPE_K8S_SNAPSHOT"
	// EnvK8sLabelSelector is a label selector matching one or more
	// VolumeSnapshots, for exercising group resolution. Empty when unset.
	EnvK8sLabelSelector = "TAPE_K8S_LABEL_SELECTOR"
	// EnvK8sDatasetParent is democratic-csi's datasetParentName, prepended to a
	// relative snapshotHandle to form the absolute ZFS path. Empty when unset.
	EnvK8sDatasetParent = "TAPE_K8S_DATASET_PARENT"
)

// K8sNamespace returns the VolumeSnapshot namespace from EnvK8sNamespace, or ""
// (cluster-wide) when unset.
func K8sNamespace(t *testing.T) string {
	t.Helper()

	return os.Getenv(EnvK8sNamespace)
}

// K8sSnapshot returns the name of a VolumeSnapshot known to exist (from
// EnvK8sSnapshot), or "" when unset.
func K8sSnapshot(t *testing.T) string {
	t.Helper()

	return os.Getenv(EnvK8sSnapshot)
}

// K8sLabelSelector returns a label selector for group resolution (from
// EnvK8sLabelSelector), or "" when unset.
func K8sLabelSelector(t *testing.T) string {
	t.Helper()

	return os.Getenv(EnvK8sLabelSelector)
}

// K8sDatasetParent returns democratic-csi's datasetParentName (from
// EnvK8sDatasetParent), or "" when unset.
func K8sDatasetParent(t *testing.T) string {
	t.Helper()

	return os.Getenv(EnvK8sDatasetParent)
}
