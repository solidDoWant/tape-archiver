//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	versioned "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// TestBackupK8sVolumeSnapshotSource backs up a Kubernetes VolumeSnapshot source
// (not a raw ZFS path), exercising the control worker's snapshot-discovery path
// end to end: it reads a bound VolumeSnapshot + VolumeSnapshotContent from the
// cluster, parses the democratic-csi snapshotHandle, maps it through
// K8sDatasetParent to a ZFS snapshot, and verifies + backs it up to tape.
//
// The fixture stands in for democratic-csi without a CSI driver: a real ZFS
// snapshot carrying the `democratic-csi:managed_resource=true` property the
// data-side Verify requires, plus pre-created **bound** VolumeSnapshot /
// VolumeSnapshotContent objects whose snapshotHandle resolves to it. A completed
// run proves resolution, the operator-granted VolumeSnapshot RBAC, the
// handle→ZFS mapping, and the data-side verify all work.
func TestBackupK8sVolumeSnapshotSource(t *testing.T) {
	h := requireHarness(t)

	// democratic-csi names the volume "pvc-<uuid>" and the snapshot
	// "snapshot-<uuid>"; the handle is "<volume>@<snapshot>" relative to
	// datasetParent, so the absolute snapshot is <parent>/pvc-<uuid>@snapshot-<uuid>.
	volume := "pvc-" + newUUID(t)
	snapshotName := "snapshot-" + newUUID(t)
	dataset := datasetParent + "/" + volume
	handle := volume + "@" + snapshotName

	createZFSSnapshotFixture(t, dataset, snapshotName)

	vsName := "e2e-vs-" + newUUID(t)
	vscName := "e2e-vsc-" + newUUID(t)
	createBoundVolumeSnapshot(t, h.snapshotClient(t), vsName, vscName, volume, handle)

	fixture := prepareBlankTapeAt(t, 3)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-k8s-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources: []config.Source{{K8s: &config.K8sRef{
			APIVersion: "snapshot.storage.k8s.io/v1",
			Kind:       "VolumeSnapshot",
			Namespace:  namespace,
			Name:       vsName,
		}}},
		Copies:     1,
		Library:    fixture.library,
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10), SliceSizeBytes: 1 << 20},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
	defer cancel()

	h.submitRun(t, cfg, runID)
	terminateOnCleanup(t, temporalClient, runID)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, runID, "").Get(runCtx, &result),
		"k8s VolumeSnapshot run must complete successfully")

	// A completed run means the k8s source resolved (VolumeSnapshot + Content read
	// under the granted RBAC, handle mapped) and the snapshot verified + backed up.
	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete for a k8s source")
	assert.Len(t, h.rec.uploadsFor(runID), 2, "report and recovery ISO must both be delivered")
}

// snapshotClient builds an external-snapshotter clientset against the kind
// cluster, used to create the fixture VolumeSnapshot objects.
func (h *e2eHarness) snapshotClient(t *testing.T) versioned.Interface {
	t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", h.kubeconfig)
	require.NoError(t, err, "load kind kubeconfig")

	c, err := versioned.NewForConfig(cfg)
	require.NoError(t, err, "build snapshot clientset")

	return c
}

// newUUID returns a fresh RFC-4122 UUID (8-4-4-4-12 hex), matching the
// democratic-csi snapshot-name pattern the handle parser requires.
func newUUID(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile("/proc/sys/kernel/random/uuid")
	require.NoError(t, err)

	return strings.TrimSpace(string(raw))
}

// createZFSSnapshotFixture creates a ZFS dataset with content and the
// democratic-csi managed-resource property, snapshots it, and triggers the
// snapshot automount so it propagates into the data container. The test process
// runs as root (via `make test-e2e`), so zfs needs no sudo. Registers teardown.
func createZFSSnapshotFixture(t *testing.T, dataset, snapshotName string) {
	t.Helper()

	runZFS(t, "create", dataset)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_ = exec.CommandContext(ctx, "zfs", "destroy", "-r", dataset).Run()
	})

	runZFS(t, "set", "democratic-csi:managed_resource=true", dataset)

	mountpoint := strings.TrimSpace(zfsValue(t, "mountpoint", dataset))
	require.NotEmpty(t, mountpoint, "dataset mountpoint")
	require.NoError(t, os.WriteFile(filepath.Join(mountpoint, "payload.bin"),
		[]byte(strings.Repeat("tape-archiver k8s source e2e payload\n", 512)), 0o644))

	runZFS(t, "snapshot", dataset+"@"+snapshotName)

	// Reading the snapshot dir triggers the on-demand automount on the host; with
	// the pool bind mount's rshared propagation it then appears in the container.
	_, _ = os.ReadDir(filepath.Join(mountpoint, ".zfs", "snapshot", snapshotName))
}

func runZFS(t *testing.T, args ...string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "zfs", args...).CombinedOutput()
	require.NoErrorf(t, err, "zfs %s: %s", strings.Join(args, " "), out)
}

func zfsValue(t *testing.T, property, dataset string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "zfs", "get", "-Hp", "-o", "value", property, dataset).Output()
	require.NoError(t, err)

	return string(out)
}

// createBoundVolumeSnapshot creates a VolumeSnapshotContent and a VolumeSnapshot
// already bound and ready-to-use, with the content carrying the CSI snapshotHandle
// — exactly the state the control worker resolves. The minimal test CRDs declare
// no status subresource, so status is persisted on create. Registers teardown.
func createBoundVolumeSnapshot(t *testing.T, c versioned.Interface, vsName, vscName, pvc, handle string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	ready := true

	content := &snapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: vscName},
		Spec: snapshotv1.VolumeSnapshotContentSpec{
			DeletionPolicy:    snapshotv1.VolumeSnapshotContentRetain,
			Driver:            "e2e.tape-archiver.test",
			Source:            snapshotv1.VolumeSnapshotContentSource{SnapshotHandle: &handle},
			VolumeSnapshotRef: corev1.ObjectReference{Name: vsName, Namespace: namespace},
		},
		Status: &snapshotv1.VolumeSnapshotContentStatus{SnapshotHandle: &handle, ReadyToUse: &ready},
	}

	_, err := c.SnapshotV1().VolumeSnapshotContents().Create(ctx, content, metav1.CreateOptions{})
	require.NoError(t, err, "create VolumeSnapshotContent")
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()

		_ = c.SnapshotV1().VolumeSnapshotContents().Delete(cctx, vscName, metav1.DeleteOptions{})
	})

	pvcName := pvc

	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: vsName, Namespace: namespace},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source: snapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: &pvcName},
		},
		Status: &snapshotv1.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: &vscName,
			ReadyToUse:                     &ready,
		},
	}

	_, err = c.SnapshotV1().VolumeSnapshots(namespace).Create(ctx, snapshot, metav1.CreateOptions{})
	require.NoError(t, err, "create VolumeSnapshot")
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()

		_ = c.SnapshotV1().VolumeSnapshots(namespace).Delete(cctx, vsName, metav1.DeleteOptions{})
	})
}
