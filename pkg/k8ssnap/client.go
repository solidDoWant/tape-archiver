package k8ssnap

import (
	"context"
	"fmt"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	versioned "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client reads the snapshot and volume objects k8s sources resolve through.
type Client interface {
	SnapshotClient
	VolumeClient
}

// clientset adapts the typed external-snapshotter and core clientsets to Client.
type clientset struct {
	snapshots versioned.Interface
	core      kubernetes.Interface
}

// NewClient builds a Client from the in-cluster config, falling back to the
// ambient kubeconfig.
func NewClient() (Client, error) {
	config, err := restConfig()
	if err != nil {
		return nil, err
	}

	snapshots, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build snapshot clientset: %w", err)
	}

	core, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build core clientset: %w", err)
	}

	return &clientset{snapshots: snapshots, core: core}, nil
}

func (c *clientset) GetPersistentVolumeClaim(ctx context.Context, namespace, name string) (*corev1.PersistentVolumeClaim, error) {
	return c.core.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
}

// ListPersistentVolumeClaims lists claims matching labelSelector (empty namespace = all).
func (c *clientset) ListPersistentVolumeClaims(ctx context.Context, namespace, labelSelector string) ([]corev1.PersistentVolumeClaim, error) {
	list, err := c.core.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}

	return list.Items, nil
}

func (c *clientset) GetPersistentVolume(ctx context.Context, name string) (*corev1.PersistentVolume, error) {
	return c.core.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
}

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

// ListVolumeSnapshots lists snapshots matching labelSelector (empty namespace = all).
func (c *clientset) ListVolumeSnapshots(ctx context.Context, namespace, labelSelector string) ([]snapshotv1.VolumeSnapshot, error) {
	list, err := c.snapshots.SnapshotV1().VolumeSnapshots(namespace).List(
		ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}

	return list.Items, nil
}
