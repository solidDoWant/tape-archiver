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

// TestBackupK8sVolumeSnapshotSource backs up a VolumeSnapshot source end to end:
// discovery under the chart's RBAC, democratic-csi handle mapping via
// K8sDatasetParent, and data-side verify. The fixture fakes democratic-csi with
// a real ZFS snapshot carrying `democratic-csi:managed_resource=true` and
// pre-bound snapshot objects.
func TestBackupK8sVolumeSnapshotSource(t *testing.T) {
	h := requireHarness(t)

	// democratic-csi's handle is "pvc-<uuid>@snapshot-<uuid>" relative to datasetParent.
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
		Redundancy: config.Redundancy{TargetPercentage: ptrFloat(10)},
		Encryption: config.Encryption{Recipients: []string{recipient}, Identity: identity},
		Delivery:   config.Delivery{WebhookURL: h.deliveryURL(runID)},
	}
	require.NoError(t, cfg.Validate(), "run config must be valid")

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Minute)
	defer cancel()

	h.submitRun(t, cfg)
	terminateOnCleanup(t, temporalClient)

	var result backup.Result
	require.NoError(t, temporalClient.GetWorkflow(runCtx, backupWorkflowID, "").Get(runCtx, &result),
		"k8s VolumeSnapshot run must complete successfully")

	assert.Equal(t, orderedPhases, result.CompletedPhases, "all ten phases must complete for a k8s source")
	assert.Len(t, h.rec.uploadsFor(runID), 1, "the report is delivered (report-only delivery, SPEC §5)")
}

// snapshotClient builds an external-snapshotter clientset for the kind cluster.
func (h *e2eHarness) snapshotClient(t *testing.T) versioned.Interface {
	t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", h.kubeconfig)
	require.NoError(t, err, "load kind kubeconfig")

	c, err := versioned.NewForConfig(cfg)
	require.NoError(t, err, "build snapshot clientset")

	return c
}

// newUUID returns an RFC-4122 UUID, as the democratic-csi handle parser requires.
func newUUID(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile("/proc/sys/kernel/random/uuid")
	require.NoError(t, err)

	return strings.TrimSpace(string(raw))
}

// createZFSSnapshotFixture creates a democratic-csi-managed dataset snapshot and
// triggers its automount. The test runs as root, so zfs needs no sudo.
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

	// Reading the dir triggers the automount, which propagates via rshared.
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

// createBoundVolumeSnapshot creates pre-bound, ready snapshot objects. The test
// CRDs have no status subresource, so status persists on create.
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
