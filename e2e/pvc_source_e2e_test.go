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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/k8ssnap"
	"github.com/solidDoWant/tape-archiver/pkg/zfs"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// TestBackupK8sPVCSource (#419) backs up a PersistentVolumeClaim label
// selector end to end: the control worker lists the claims cluster-wide under
// the chart's RBAC and follows each static, democratic-csi-shaped PV to its
// dataset; the data worker re-checks managed_resource and snapshots each one.
// Two claims share a name in different namespaces, so each must get its own
// namespace-qualified archive.
func TestBackupK8sPVCSource(t *testing.T) {
	h := requireHarness(t)
	core := h.coreClient(t)

	selector := "tape-archiver.test/pvc-e2e=" + strings.ReplaceAll(newUUID(t), "-", "")[:16]
	otherNamespace := "ta-e2e-pvc-" + strings.ReplaceAll(newUUID(t), "-", "")[:8]
	createNamespace(t, core, otherNamespace)

	datasets := make([]string, 0, 2)

	for _, claimNamespace := range []string{namespace, otherNamespace} {
		volume := "pvc-" + newUUID(t)
		dataset := datasetParent + "/" + volume
		createManagedDataset(t, dataset)
		createBoundClaim(t, core, claimNamespace, "data", volume, selector)

		datasets = append(datasets, dataset)
	}

	fixture := prepareBlankTapeAt(t, 35)
	temporalClient := dialTemporal(t)
	identity, recipient := generateTestKeypair(t)

	runID := fmt.Sprintf("e2e-pvc-%d", time.Now().UnixNano())

	cfg := config.Config{
		Sources: []config.Source{{K8s: &config.K8sRef{
			APIVersion: "v1", Kind: config.KindPVC, LabelSelector: selector,
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
		"PersistentVolumeClaim run must complete successfully")
	assert.Equal(t, orderedPhases, result.CompletedPhases)

	uploads := h.rec.uploadsFor(runID)
	require.Len(t, uploads, 1)

	snapshot := backup.RunSnapshotName(temporalRunID(t, temporalClient))
	report := strings.Join(strings.Fields(extractPDFText(t, findUpload(t, uploads, "report.pdf"))), "")

	for index, dataset := range datasets {
		claimNamespace := []string{namespace, otherNamespace}[index]

		assert.Contains(t, report, dataset+"@"+snapshot, "each claim is staged from its own run-scoped snapshot")
		assert.Contains(t, report, claimNamespace+"-data", "each claim has its own namespace-qualified archive")

		snapshots, err := zfs.Snapshots(t.Context(), dataset)
		require.NoError(t, err)

		for _, existing := range snapshots {
			assert.NotEqual(t, snapshot, existing.Name, "the run-scoped snapshot is destroyed after the run")
		}
	}
}

// coreClient builds a core clientset for the kind cluster.
func (h *e2eHarness) coreClient(t *testing.T) kubernetes.Interface {
	t.Helper()

	cfg, err := clientcmd.BuildConfigFromFlags("", h.kubeconfig)
	require.NoError(t, err, "load kind kubeconfig")

	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err, "build core clientset")

	return client
}

func createNamespace(t *testing.T, core kubernetes.Interface, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	_, err := core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err, "create namespace")
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()

		_ = core.CoreV1().Namespaces().Delete(cctx, name, metav1.DeleteOptions{})
	})
}

// createManagedDataset creates a democratic-csi-shaped dataset with a payload.
func createManagedDataset(t *testing.T, dataset string) {
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
		[]byte(strings.Repeat("tape-archiver PVC source e2e payload\n", 512)), 0o644))
}

// createBoundClaim creates a static PV shaped like a democratic-csi
// zfs-generic-nfs volume (its handle is the dataset name relative to the
// parent) and a claim pre-bound to it, and waits for the claim to bind.
func createBoundClaim(t *testing.T, core kubernetes.Interface, claimNamespace, name, volume, selector string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	label, value, _ := strings.Cut(selector, "=")
	size := resource.MustParse("1Gi")
	pvName := "e2e-" + volume

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: size},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef:                      &corev1.ObjectReference{Namespace: claimNamespace, Name: name},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: k8ssnap.DefaultCSIDriver, VolumeHandle: volume},
			},
		},
	}

	_, err := core.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
	require.NoError(t, err, "create PersistentVolume")
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()

		_ = core.CoreV1().PersistentVolumes().Delete(cctx, pvName, metav1.DeleteOptions{})
	})

	emptyClass := ""
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: claimNamespace, Name: name, Labels: map[string]string{label: value}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
			StorageClassName: &emptyClass,
			VolumeName:       pvName,
		},
	}

	_, err = core.CoreV1().PersistentVolumeClaims(claimNamespace).Create(ctx, claim, metav1.CreateOptions{})
	require.NoError(t, err, "create PersistentVolumeClaim")
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()

		_ = core.CoreV1().PersistentVolumeClaims(claimNamespace).Delete(cctx, name, metav1.DeleteOptions{})
	})

	require.Eventually(t, func() bool {
		got, err := core.CoreV1().PersistentVolumeClaims(claimNamespace).Get(ctx, name, metav1.GetOptions{})

		return err == nil && got.Status.Phase == corev1.ClaimBound
	}, 60*time.Second, time.Second, "PersistentVolumeClaim %s/%s must bind to its static PV", claimNamespace, name)
}
