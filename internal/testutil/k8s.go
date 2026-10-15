package testutil

import (
	"os"
	"testing"
)

const (
	// EnvK8sNamespace is the namespace of the VolumeSnapshot under test.
	EnvK8sNamespace = "TAPE_K8S_NAMESPACE"
	// EnvK8sSnapshot names a VolumeSnapshot known to exist.
	EnvK8sSnapshot = "TAPE_K8S_SNAPSHOT"
	// EnvK8sLabelSelector matches one or more VolumeSnapshots.
	EnvK8sLabelSelector = "TAPE_K8S_LABEL_SELECTOR"
	// EnvK8sDatasetParent is democratic-csi's datasetParentName.
	EnvK8sDatasetParent = "TAPE_K8S_DATASET_PARENT"
)

// K8sNamespace returns EnvK8sNamespace, or "" (cluster-wide).
func K8sNamespace(t *testing.T) string {
	t.Helper()

	return os.Getenv(EnvK8sNamespace)
}

// K8sSnapshot returns EnvK8sSnapshot, or "".
func K8sSnapshot(t *testing.T) string {
	t.Helper()

	return os.Getenv(EnvK8sSnapshot)
}

// K8sLabelSelector returns EnvK8sLabelSelector, or "".
func K8sLabelSelector(t *testing.T) string {
	t.Helper()

	return os.Getenv(EnvK8sLabelSelector)
}

// K8sDatasetParent returns EnvK8sDatasetParent, or "".
func K8sDatasetParent(t *testing.T) string {
	t.Helper()

	return os.Getenv(EnvK8sDatasetParent)
}
