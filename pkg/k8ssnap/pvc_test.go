package k8ssnap

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const testDriver = "org.democratic-csi.nfs"

// boundClaim builds a claim bound to a democratic-csi volume with handle.
func boundClaim(namespace, name, volume, handle string, labels map[string]string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volume},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: volume},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Namespace: namespace, Name: name},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: testDriver, VolumeHandle: handle},
			},
		},
	}

	return claim, pv
}

// fakeVolumeClient is the real clientset adapter over client-go's fake clientset.
func fakeVolumeClient(objects ...runtime.Object) VolumeClient {
	return &clientset{core: fake.NewClientset(objects...)}
}

func TestResolveClaim(t *testing.T) {
	t.Parallel()

	claim, pv := boundClaim("media", "library", "pvc-1", "pvc-1", nil)

	tests := []struct {
		name        string
		mutate      func(*corev1.PersistentVolumeClaim, *corev1.PersistentVolume)
		driver      string
		want        Claim
		assertErr   require.ErrorAssertionFunc
		unsupported bool
	}{
		{
			name: "bound democratic-csi volume",
			want: Claim{Namespace: "media", Name: "library", Volume: "pvc-1", Dataset: "pool/k8s/pvc-1"},
		},
		{
			name:   "explicit driver",
			mutate: func(_ *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) { pv.Spec.CSI.Driver = "csi.example" },
			driver: "csi.example",
			want:   Claim{Namespace: "media", Name: "library", Volume: "pvc-1", Dataset: "pool/k8s/pvc-1"},
		},
		{
			name: "unbound claim",
			mutate: func(claim *corev1.PersistentVolumeClaim, _ *corev1.PersistentVolume) {
				claim.Status.Phase = corev1.ClaimPending
			},
			unsupported: true,
		},
		{
			name: "volume bound to another claim",
			mutate: func(_ *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) {
				pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: "other", Name: "library"}
			},
			unsupported: true,
		},
		{
			name: "not a CSI volume",
			mutate: func(_ *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) {
				pv.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nas", Path: "/x"}}
			},
			unsupported: true,
		},
		{
			name: "other CSI driver",
			mutate: func(_ *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) {
				pv.Spec.CSI.Driver = "ebs.csi.aws.com"
			},
			unsupported: true,
		},
		{
			name: "snapshot-shaped handle",
			mutate: func(_ *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) {
				pv.Spec.CSI.VolumeHandle = "pvc-1@snap"
			},
			unsupported: true,
		},
		{
			name: "escaping handle",
			mutate: func(_ *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) {
				pv.Spec.CSI.VolumeHandle = "../other"
			},
			unsupported: true,
		},
		{
			name: "missing volume is retryable",
			mutate: func(claim *corev1.PersistentVolumeClaim, _ *corev1.PersistentVolume) {
				claim.Spec.VolumeName = "pvc-gone"
			},
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
				if test.unsupported {
					test.assertErr = require.Error
				}
			}

			claim, pv := claim.DeepCopy(), pv.DeepCopy()
			if test.mutate != nil {
				test.mutate(claim, pv)
			}

			resolver := NewClaimResolver(fakeVolumeClient(claim, pv), "pool/k8s/", test.driver)

			got, err := resolver.ResolveClaim(t.Context(), ClaimRef{Namespace: "media", Name: "library"})
			test.assertErr(t, err)
			assert.Equal(t, test.want, got)

			if err != nil {
				assert.ErrorContains(t, err, "media/library", "the error names the claim")
				assert.Equal(t, test.unsupported, errorIsUnsupported(err))
			}
		})
	}
}

func errorIsUnsupported(err error) bool {
	return errors.Is(err, ErrUnsupportedClaim)
}

func TestListClaims(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"tape": "true"}
	first, _ := boundClaim("media", "library", "pvc-1", "pvc-1", labels)
	second, _ := boundClaim("apps", "library", "pvc-2", "pvc-2", labels)
	third, _ := boundClaim("apps", "db", "pvc-3", "pvc-3", labels)
	unlabelled, _ := boundClaim("apps", "cache", "pvc-4", "pvc-4", nil)

	resolver := NewClaimResolver(fakeVolumeClient(first, second, third, unlabelled), "", "")

	all, err := resolver.ListClaims(t.Context(), "", "tape=true")
	require.NoError(t, err)
	assert.Equal(t, []ClaimRef{{"apps", "db"}, {"apps", "library"}, {"media", "library"}}, all, "cluster-wide, sorted")

	scoped, err := resolver.ListClaims(t.Context(), "media", "tape=true")
	require.NoError(t, err)
	assert.Equal(t, []ClaimRef{{"media", "library"}}, scoped)

	_, err = resolver.ListClaims(t.Context(), "", "tape=nope")
	require.ErrorIs(t, err, ErrNoClaims)
}

func TestVerifyDataset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		properties map[string]string
		assertErr  require.ErrorAssertionFunc
	}{
		{name: "managed", properties: map[string]string{managedResourceProperty: "true"}, assertErr: require.NoError},
		{name: "unmanaged", assertErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			reader := &fakePropertyReader{properties: test.properties}
			test.assertErr(t, VerifyDataset(t.Context(), reader, "pool/k8s/pvc-1"))
			assert.Equal(t, "pool/k8s/pvc-1", reader.askedPath)
		})
	}
}
