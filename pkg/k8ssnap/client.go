package k8ssnap

import (
	"context"
	"fmt"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	versioned "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// clientset adapts the typed external-snapshotter clientset to SnapshotClient.
type clientset struct {
	snapshots versioned.Interface
}

// NewClient builds a SnapshotClient from the ambient Kubernetes configuration:
// the in-cluster config when running as a pod (the control worker), otherwise the
// kubeconfig named by KUBECONFIG or found by the default loading rules. Per issue
// #13's non-goals it does no kubeconfig bootstrapping of its own.
func NewClient() (SnapshotClient, error) {
	config, err := restConfig()
	if err != nil {
		return nil, err
	}

	snapshots, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build snapshot clientset: %w", err)
	}

	return &clientset{snapshots: snapshots}, nil
}

// restConfig prefers the in-cluster config and falls back to the ambient
// kubeconfig (KUBECONFIG or the default loading rules).
func restConfig() (*rest.Config, error) {
	if config, err := rest.InClusterConfig(); err == nil {
		return config, nil
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	return config, nil
}

func (c *clientset) GetVolumeSnapshot(ctx context.Context, namespace, name string) (*snapshotv1.VolumeSnapshot, error) {
	return c.snapshots.SnapshotV1().VolumeSnapshots(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *clientset) GetVolumeSnapshotContent(ctx context.Context, name string) (*snapshotv1.VolumeSnapshotContent, error) {
	return c.snapshots.SnapshotV1().VolumeSnapshotContents().Get(ctx, name, metav1.GetOptions{})
}

// ListVolumeSnapshots lists snapshots in namespace (empty means all namespaces)
// matching labelSelector.
func (c *clientset) ListVolumeSnapshots(ctx context.Context, namespace, labelSelector string) ([]snapshotv1.VolumeSnapshot, error) {
	list, err := c.snapshots.SnapshotV1().VolumeSnapshots(namespace).List(
		ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}

	return list.Items, nil
}
