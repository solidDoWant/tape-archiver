package k8ssnap

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// DefaultCSIDriver is the CSI driver name of democratic-csi's zfs-generic-nfs
// deployment as its example values name it; override it with the deployment's
// csiDriver.name when that differs.
const DefaultCSIDriver = "org.democratic-csi.nfs"

var (
	// ErrNoClaims reports that a label selector matched no claims.
	ErrNoClaims = errors.New("no PersistentVolumeClaims matched")
	// ErrUnsupportedClaim reports a claim that is not a bound democratic-csi
	// zfs-generic-nfs volume; retrying cannot fix it.
	ErrUnsupportedClaim = errors.New("not a bound democratic-csi zfs-generic-nfs volume")
)

// VolumeClient reads PersistentVolumeClaims and PersistentVolumes.
type VolumeClient interface {
	GetPersistentVolumeClaim(ctx context.Context, namespace, name string) (*corev1.PersistentVolumeClaim, error)
	ListPersistentVolumeClaims(ctx context.Context, namespace, labelSelector string) ([]corev1.PersistentVolumeClaim, error)
	GetPersistentVolume(ctx context.Context, name string) (*corev1.PersistentVolume, error)
}

// ClaimRef names one PersistentVolumeClaim.
type ClaimRef struct {
	Namespace string
	Name      string
}

// Claim is a PersistentVolumeClaim resolved to its democratic-csi ZFS dataset.
type Claim struct {
	Namespace string
	Name      string // PVC name
	Volume    string // bound PersistentVolume name
	Dataset   string // absolute ZFS dataset path
}

// ClaimResolver maps PersistentVolumeClaims to democratic-csi ZFS datasets.
type ClaimResolver struct {
	client        VolumeClient
	datasetParent string
	driver        string
}

// NewClaimResolver returns a ClaimResolver accepting only volumes of the CSI
// driver named driver (DefaultCSIDriver when empty); datasetParent is
// democratic-csi's datasetParentName, which the volumeHandle omits.
func NewClaimResolver(client VolumeClient, datasetParent, driver string) *ClaimResolver {
	if driver == "" {
		driver = DefaultCSIDriver
	}

	return &ClaimResolver{client: client, datasetParent: datasetParent, driver: driver}
}

// ListClaims returns the claims matching labelSelector, sorted by namespace
// then name (empty namespace means cluster-wide). Zero matches wraps ErrNoClaims.
func (r *ClaimResolver) ListClaims(ctx context.Context, namespace, labelSelector string) ([]ClaimRef, error) {
	claims, err := r.client.ListPersistentVolumeClaims(ctx, namespace, labelSelector)
	if err != nil {
		return nil, fmt.Errorf("list PersistentVolumeClaims (namespace %q, selector %q): %w", namespace, labelSelector, err)
	}

	if len(claims) == 0 {
		return nil, fmt.Errorf("label selector %q (namespace %q): %w", labelSelector, namespace, ErrNoClaims)
	}

	refs := make([]ClaimRef, 0, len(claims))
	for _, claim := range claims {
		refs = append(refs, ClaimRef{Namespace: claim.Namespace, Name: claim.Name})
	}

	slices.SortFunc(refs, func(a, b ClaimRef) int {
		if order := strings.Compare(a.Namespace, b.Namespace); order != 0 {
			return order
		}

		return strings.Compare(a.Name, b.Name)
	})

	return refs, nil
}

// ResolveClaim follows a bound claim to its PersistentVolume and returns the
// dataset named by the volume's CSI volumeHandle. Anything but a bound volume
// of the expected CSI driver wraps ErrUnsupportedClaim and names the claim.
func (r *ClaimResolver) ResolveClaim(ctx context.Context, ref ClaimRef) (Claim, error) {
	id := ref.Namespace + "/" + ref.Name

	claim, err := r.client.GetPersistentVolumeClaim(ctx, ref.Namespace, ref.Name)
	if err != nil {
		return Claim{}, fmt.Errorf("get PersistentVolumeClaim %s: %w", id, err)
	}

	if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" {
		return Claim{}, fmt.Errorf("PersistentVolumeClaim %s is not bound (phase %q): %w", id, claim.Status.Phase, ErrUnsupportedClaim)
	}

	volume, err := r.client.GetPersistentVolume(ctx, claim.Spec.VolumeName)
	if err != nil {
		return Claim{}, fmt.Errorf("get PersistentVolume %s for PersistentVolumeClaim %s: %w", claim.Spec.VolumeName, id, err)
	}

	// The UID guards against a claim deleted and recreated under the same name.
	if back := volume.Spec.ClaimRef; back == nil || back.Namespace != claim.Namespace || back.Name != claim.Name || back.UID != claim.UID {
		return Claim{}, fmt.Errorf("PersistentVolumeClaim %s: PersistentVolume %s is not bound back to it: %w", id, volume.Name, ErrUnsupportedClaim)
	}

	csi := volume.Spec.CSI
	if csi == nil {
		return Claim{}, fmt.Errorf("PersistentVolumeClaim %s: PersistentVolume %s is not a CSI volume: %w", id, volume.Name, ErrUnsupportedClaim)
	}

	if csi.Driver != r.driver {
		return Claim{}, fmt.Errorf("PersistentVolumeClaim %s: PersistentVolume %s uses CSI driver %q, expected %q: %w",
			id, volume.Name, csi.Driver, r.driver, ErrUnsupportedClaim)
	}

	handle := csi.VolumeHandle
	if !validVolumeHandle(handle) {
		return Claim{}, fmt.Errorf("PersistentVolumeClaim %s: PersistentVolume %s has an unusable volumeHandle %q: %w",
			id, volume.Name, handle, ErrUnsupportedClaim)
	}

	return Claim{
		Namespace: claim.Namespace,
		Name:      claim.Name,
		Volume:    volume.Name,
		Dataset:   absoluteDataset(r.datasetParent, handle),
	}, nil
}

// handleComponentPattern is one relative dataset path component: ZFS name
// characters, not starting with "-" (a zfs option) or "." ("." / "..").
var handleComponentPattern = regexp.MustCompile(`^[A-Za-z0-9_:][A-Za-z0-9_:.-]*$`)

// validVolumeHandle accepts only a relative dataset path, so prefixing it with
// the dataset parent cannot escape the parent or name a snapshot.
func validVolumeHandle(handle string) bool {
	for component := range strings.SplitSeq(handle, "/") {
		if !handleComponentPattern.MatchString(component) {
			return false
		}
	}

	return true
}

// VerifyDataset confirms dataset exists and is democratic-csi-managed before
// it is snapshotted and staged (SPEC.md §4.3, §16).
func VerifyDataset(ctx context.Context, reader PropertyReader, dataset string) error {
	managed, err := reader.UserProperty(ctx, dataset, managedResourceProperty)
	if err != nil {
		return fmt.Errorf("read user property %s for %s: %w", managedResourceProperty, dataset, err)
	}

	if managed != "true" {
		return fmt.Errorf("dataset %s is not democratic-csi managed (%s=%q)", dataset, managedResourceProperty, managed)
	}

	return nil
}
