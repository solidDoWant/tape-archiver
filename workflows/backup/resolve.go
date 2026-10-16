package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/k8ssnap"
	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// The Resolve phase (SPEC §4.3 phase 1) expands the config into a work list and
// runs the feasibility pre-check, split by the SPEC §16 ownership rule: k8s
// resolution on the control worker (where the API is reachable), ZFS validation
// and sizing on the data worker (where the pool is).

const (
	resolveControlTimeout = 5 * time.Minute
	resolveDataTimeout    = 10 * time.Minute

	// resolveMaxAttempts bounds retries: Temporal's default is unlimited, so a
	// deterministic failure would never fail the run or fire the alert (#385).
	resolveMaxAttempts = 3

	// resolveInfeasibleErrorType marks a source that cannot fit one tape.
	resolveInfeasibleErrorType = "resolve-infeasible"

	// resolveNoMatchErrorType marks a selector source that matched nothing.
	resolveNoMatchErrorType = "resolve-no-match"

	// resolveUnsupportedClaimErrorType marks a PersistentVolumeClaim that is not
	// a bound democratic-csi zfs-generic-nfs volume.
	resolveUnsupportedClaimErrorType = "resolve-unsupported-claim"

	// resolveLabelCollisionErrorType marks two claims whose derived archive
	// labels collide.
	resolveLabelCollisionErrorType = "resolve-label-collision"
)

// snapshotResolver is the seam *k8ssnap.Resolver satisfies, injected for tests.
type snapshotResolver interface {
	Resolve(ctx context.Context, ref k8ssnap.Ref) (k8ssnap.Snapshot, error)
	ResolveGroup(ctx context.Context, ref k8ssnap.Ref) (k8ssnap.Group, error)
}

// poolInspector reads ZFS properties; injected for tests. UserProperty also
// satisfies k8ssnap.PropertyReader. UserProperties is only an existence check (zfs
// exits non-zero for an absent dataset); its values are never trusted.
type poolInspector interface {
	UserProperties(ctx context.Context, dataset string) (map[string]string, error)
	UserProperty(ctx context.Context, dataset, property string) (string, error)
	LogicalReferenced(ctx context.Context, dataset string) (int64, error)
	Snapshots(ctx context.Context, dataset string) ([]zfs.SnapshotInfo, error)
}

// zfsPool is the production poolInspector over the zfs CLI.
type zfsPool struct{}

func (zfsPool) UserProperties(ctx context.Context, dataset string) (map[string]string, error) {
	return zfs.UserProperties(ctx, dataset)
}

func (zfsPool) UserProperty(ctx context.Context, dataset, property string) (string, error) {
	return zfs.UserProperty(ctx, dataset, property)
}

func (zfsPool) LogicalReferenced(ctx context.Context, dataset string) (int64, error) {
	return zfs.LogicalReferenced(ctx, dataset)
}

func (zfsPool) Snapshots(ctx context.Context, dataset string) ([]zfs.SnapshotInfo, error) {
	return zfs.Snapshots(ctx, dataset)
}

// claimResolver is the seam *k8ssnap.ClaimResolver satisfies, injected for tests.
type claimResolver interface {
	ListClaims(ctx context.Context, namespace, labelSelector string) ([]k8ssnap.ClaimRef, error)
	ResolveClaim(ctx context.Context, ref k8ssnap.ClaimRef) (k8ssnap.Claim, error)
}

// ResolveControlActivities hosts the control-side Resolve activities.
type ResolveControlActivities struct {
	// The factories build the Kubernetes client only when a run has k8s
	// sources; raw-ZFS-only runs need no cluster access.
	newResolver      func() (snapshotResolver, error)
	newClaimResolver func() (claimResolver, error)
}

func newResolveControlActivities(datasetParent, csiDriver string) *ResolveControlActivities {
	return &ResolveControlActivities{
		newResolver: func() (snapshotResolver, error) {
			client, err := k8ssnap.NewClient()
			if err != nil {
				return nil, err
			}

			return k8ssnap.NewResolver(client, datasetParent), nil
		},
		newClaimResolver: func() (claimResolver, error) {
			client, err := k8ssnap.NewClient()
			if err != nil {
				return nil, err
			}

			return k8ssnap.NewClaimResolver(client, datasetParent, csiDriver), nil
		},
	}
}

// ClaimMatches is the claims one PersistentVolumeClaim labelSelector source
// matched.
type ClaimMatches struct {
	SourceIndex int
	Claims      []k8ssnap.ClaimRef
}

// ListClaimSources lists the claims every PersistentVolumeClaim labelSelector
// source matches; a source matching nothing fails naming it.
func (a *ResolveControlActivities) ListClaimSources(ctx context.Context, cfg config.Config) ([]ClaimMatches, error) {
	var (
		resolver claimResolver
		matches  []ClaimMatches
	)

	for index, source := range cfg.Sources {
		if !isClaimSelector(source) {
			continue
		}

		if resolver == nil {
			built, err := a.newClaimResolver()
			if err != nil {
				return nil, fmt.Errorf("build k8s resolver: %w", err)
			}

			resolver = built
		}

		claims, err := resolver.ListClaims(ctx, source.K8s.Namespace, source.K8s.LabelSelector)
		if err != nil {
			err = fmt.Errorf("resolve sources[%d] %s: %w", index, config.KindPVC, err)
			if errors.Is(err, k8ssnap.ErrNoClaims) {
				return nil, temporal.NewNonRetryableApplicationError(err.Error(), resolveNoMatchErrorType, err)
			}

			return nil, err
		}

		slog.InfoContext(ctx, "resolve: selected PersistentVolumeClaims", "sourceIndex", index, "claims", claims)

		matches = append(matches, ClaimMatches{SourceIndex: index, Claims: claims})
	}

	return matches, nil
}

func isClaim(source config.Source) bool {
	return source.K8s != nil && source.K8s.Kind == config.KindPVC
}

func isClaimSelector(source config.Source) bool {
	return isClaim(source) && source.K8s.LabelSelector != ""
}

// expandClaimSources replaces each PersistentVolumeClaim labelSelector source
// with one named source per matched claim, then labels every unlabelled claim
// source whose name another namespace also uses "<namespace>-<name>". Derived
// labels that still collide (truncation) fail rather than leave two archives a
// recoverer cannot tell apart.
func expandClaimSources(sources []config.Source, matches []ClaimMatches) ([]config.Source, error) {
	byIndex := make(map[int][]k8ssnap.ClaimRef, len(matches))
	for _, match := range matches {
		byIndex[match.SourceIndex] = match.Claims
	}

	expanded := make([]config.Source, 0, len(sources))

	for index, source := range sources {
		if !isClaimSelector(source) {
			expanded = append(expanded, source)

			continue
		}

		for _, claim := range byIndex[index] {
			expanded = append(expanded, config.Source{
				Compression: source.Compression,
				K8s:         &config.K8sRef{APIVersion: source.K8s.APIVersion, Kind: config.KindPVC, Namespace: claim.Namespace, Name: claim.Name},
			})
		}
	}

	namespaces := make(map[string]map[string]struct{})

	for _, source := range expanded {
		if isClaim(source) {
			if namespaces[source.K8s.Name] == nil {
				namespaces[source.K8s.Name] = make(map[string]struct{})
			}

			namespaces[source.K8s.Name][source.K8s.Namespace] = struct{}{}
		}
	}

	labelOwner := make(map[string]string)

	for index, source := range expanded {
		// An operator-chosen label is theirs to keep distinct.
		if !isClaim(source) || source.Label != nil {
			continue
		}

		claim := source.K8s.Namespace + "/" + source.K8s.Name

		if len(namespaces[source.K8s.Name]) > 1 {
			label := source.K8s.Namespace + "-" + source.K8s.Name
			expanded[index].Label = &label
		}

		label := sourceLabel(expanded[index])
		if owner, ok := labelOwner[label]; ok && owner != claim {
			return nil, fmt.Errorf("PersistentVolumeClaims %s and %s would both be archived as %q; "+
				"name one of them as its own source with a distinct label", owner, claim, label)
		}

		labelOwner[label] = claim
	}

	return expanded, nil
}

// ResolveK8sSources resolves every k8s source to its ZFS snapshot(s); a
// LabelSelector source becomes one grouped archive (SPEC §5). Raw ZFS sources are
// left to the data worker.
func (a *ResolveControlActivities) ResolveK8sSources(ctx context.Context, cfg config.Config) ([]ResolvedArchive, error) {
	var (
		resolver       snapshotResolver
		claimsResolver claimResolver
		resolved       []ResolvedArchive
	)

	for index, source := range cfg.Sources {
		if source.K8s == nil {
			continue
		}

		if isClaim(source) {
			if claimsResolver == nil {
				built, err := a.newClaimResolver()
				if err != nil {
					return nil, fmt.Errorf("build k8s resolver: %w", err)
				}

				claimsResolver = built
			}

			archive, err := resolveClaimSource(ctx, claimsResolver, index, source)
			if err != nil {
				return nil, err
			}

			resolved = append(resolved, archive)

			continue
		}

		if resolver == nil {
			built, err := a.newResolver()
			if err != nil {
				return nil, fmt.Errorf("build k8s resolver: %w", err)
			}

			resolver = built
		}

		archive, err := resolveK8sSource(ctx, resolver, index, source)
		if err != nil {
			return nil, err
		}

		resolved = append(resolved, archive)

		slog.InfoContext(ctx, "resolve: resolved k8s source to ZFS snapshot(s)",
			"sourceIndex", index, "label", archive.Label, "snapshots", len(archive.Snapshots))
	}

	slog.InfoContext(ctx, "resolve: resolved all k8s sources on the control worker", "k8sSources", len(resolved))

	return resolved, nil
}

func resolveK8sSource(ctx context.Context, resolver snapshotResolver, index int, source config.Source) (ResolvedArchive, error) {
	k8s := source.K8s
	ref := k8ssnap.Ref{
		Namespace:     k8s.Namespace,
		Name:          k8s.Name,
		LabelSelector: k8s.LabelSelector,
		NewestPerPVC:  k8s.Select == config.SelectNewestPerPVC,
	}
	archive := ResolvedArchive{SourceIndex: index, Label: sourceLabel(source), Compression: compressionEnabled(source)}

	if k8s.LabelSelector != "" {
		group, err := resolver.ResolveGroup(ctx, ref)
		if err != nil {
			return ResolvedArchive{}, fmt.Errorf("resolve sources[%d] k8s group: %w", index, err)
		}

		if len(group.UnreadyPVCs) > 0 {
			slog.WarnContext(ctx, "resolve: skipping PVCs with no ready snapshot",
				"sourceIndex", index, "pvcs", group.UnreadyPVCs)
		}

		for _, member := range group.Members {
			archive.Snapshots = append(archive.Snapshots, snapshotFromK8s(member))
		}

		return archive, nil
	}

	snapshot, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return ResolvedArchive{}, fmt.Errorf("resolve sources[%d] k8s snapshot: %w", index, err)
	}

	archive.Snapshots = append(archive.Snapshots, snapshotFromK8s(snapshot))

	return archive, nil
}

// resolveClaimSource resolves a named PersistentVolumeClaim source to its live
// dataset, which the run then snapshots (planRunScopedSnapshots). A claim the
// resolver rejects fails the run without retrying.
func resolveClaimSource(ctx context.Context, resolver claimResolver, index int, source config.Source) (ResolvedArchive, error) {
	if source.K8s.Name == "" {
		return ResolvedArchive{}, fmt.Errorf("internal: sources[%d] %s labelSelector was not expanded", index, config.KindPVC)
	}

	claim, err := resolver.ResolveClaim(ctx, k8ssnap.ClaimRef{Namespace: source.K8s.Namespace, Name: source.K8s.Name})
	if err != nil {
		err = fmt.Errorf("resolve sources[%d] %s: %w", index, config.KindPVC, err)
		if errors.Is(err, k8ssnap.ErrUnsupportedClaim) {
			return ResolvedArchive{}, temporal.NewNonRetryableApplicationError(err.Error(), resolveUnsupportedClaimErrorType, err)
		}

		return ResolvedArchive{}, err
	}

	slog.InfoContext(ctx, "resolve: resolved PersistentVolumeClaim to its dataset",
		"sourceIndex", index, "namespace", claim.Namespace, "pvc", claim.Name, "volume", claim.Volume, "dataset", claim.Dataset)

	return ResolvedArchive{
		SourceIndex: index,
		Label:       sourceLabel(source),
		Compression: compressionEnabled(source),
		Snapshots:   []ResolvedSnapshot{{ZFSPath: claim.Dataset, Namespace: claim.Namespace, PVC: claim.Name}},
	}, nil
}

func snapshotFromK8s(snapshot k8ssnap.Snapshot) ResolvedSnapshot {
	return ResolvedSnapshot{
		ZFSPath:        snapshot.ZFSPath(),
		Dataset:        snapshot.Dataset,
		SnapshotName:   snapshot.SnapshotName,
		Namespace:      snapshot.Namespace,
		VolumeSnapshot: snapshot.VolumeSnapshot,
		PVC:            snapshot.PVC,
	}
}

// ResolveDataInput is the data-side Resolve payload: the config plus the
// control-resolved k8s archives, keyed by SourceIndex.
type ResolveDataInput struct {
	Config      config.Config
	K8sArchives []ResolvedArchive
}

// ResolveDataActivities hosts the data-side Resolve activities.
type ResolveDataActivities struct {
	pool     poolInspector
	selector datasetSelector
	// stagingRoot's dataset is never selected by a zfsProperty source.
	stagingRoot string
}

func newResolveDataActivities(stagingRoot string) *ResolveDataActivities {
	return &ResolveDataActivities{pool: zfsPool{}, selector: zfsSelector{}, stagingRoot: stagingRoot}
}

// datasetSelector finds the datasets a zfsProperty source selects; injected
// for tests.
type datasetSelector interface {
	LocalProperty(ctx context.Context, property, datasetType, root string) ([]zfs.PropertyRecord, error)
	UserProperty(ctx context.Context, name, property string) (string, error)
	PropertySource(ctx context.Context, name, property string) (string, error)
	Filesystems(ctx context.Context) ([]zfs.Filesystem, error)
}

type zfsSelector struct{}

func (zfsSelector) LocalProperty(ctx context.Context, property, datasetType, root string) ([]zfs.PropertyRecord, error) {
	return zfs.LocalProperty(ctx, property, datasetType, root)
}

func (zfsSelector) UserProperty(ctx context.Context, name, property string) (string, error) {
	return zfs.UserProperty(ctx, name, property)
}

func (zfsSelector) PropertySource(ctx context.Context, name, property string) (string, error) {
	return zfs.PropertySource(ctx, name, property)
}

func (zfsSelector) Filesystems(ctx context.Context) ([]zfs.Filesystem, error) {
	return zfs.Filesystems(ctx)
}

// PropertyMatches is the datasets one zfsProperty source selected.
type PropertyMatches struct {
	SourceIndex int
	Datasets    []string
}

// ResolvePropertySources resolves every zfsProperty source to the filesystems
// with the property set locally to the value (inherited values never match),
// excluding the staging dataset and its descendants. A source matching nothing
// fails naming it.
func (a *ResolveDataActivities) ResolvePropertySources(ctx context.Context, cfg config.Config) ([]PropertyMatches, error) {
	stagingDataset, err := a.stagingDataset(ctx)
	if err != nil {
		return nil, err
	}

	var matches []PropertyMatches

	for index, source := range cfg.Sources {
		if source.ZFSProperty == nil {
			continue
		}

		datasets, err := a.selectDatasets(ctx, *source.ZFSProperty, stagingDataset)
		if err != nil {
			return nil, fmt.Errorf("resolve sources[%d] zfsProperty %s: %w", index, source.ZFSProperty.Name, err)
		}

		if len(datasets) == 0 {
			property := source.ZFSProperty

			return nil, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"sources[%d] zfsProperty %s=%s (under %q) matched no datasets with the property set locally",
				index, property.Name, property.Value, property.Under), resolveNoMatchErrorType, nil)
		}

		slog.InfoContext(ctx, "resolve: selected datasets by ZFS property",
			"sourceIndex", index, "property", source.ZFSProperty.Name, "datasets", datasets)

		matches = append(matches, PropertyMatches{SourceIndex: index, Datasets: datasets})
	}

	return matches, nil
}

// stagingDataset is the filesystem holding the staging directory, or "" when
// no staging directory is configured or it is not on ZFS.
func (a *ResolveDataActivities) stagingDataset(ctx context.Context) (string, error) {
	if a.stagingRoot == "" {
		return "", nil
	}

	filesystems, err := a.selector.Filesystems(ctx)
	if err != nil {
		return "", fmt.Errorf("find the staging directory's dataset: %w", err)
	}

	return zfs.ContainingFilesystem(filesystems, a.stagingRoot), nil
}

func (a *ResolveDataActivities) selectDatasets(ctx context.Context, property config.ZFSPropertySource, stagingDataset string) ([]string, error) {
	records, err := a.selector.LocalProperty(ctx, property.Name, "filesystem", property.Under)
	if err != nil {
		return nil, err
	}

	var datasets []string

	for _, record := range records {
		// A fabricated record may name a snapshot or a dataset outside Under.
		if record.Value != property.Value || !datasetWithin(record.Name, property.Under) {
			continue
		}

		if stagingDataset != "" && (record.Name == stagingDataset || strings.HasPrefix(record.Name, stagingDataset+"/")) {
			slog.WarnContext(ctx, "resolve: not selecting the staging dataset or a dataset under it",
				"dataset", record.Name, "stagingDataset", stagingDataset)

			continue
		}

		// A value with an embedded newline can fabricate a listed record, so
		// confirm each with single-property reads.
		value, err := a.selector.UserProperty(ctx, record.Name, property.Name)
		if err != nil {
			return nil, err
		}

		source, err := a.selector.PropertySource(ctx, record.Name, property.Name)
		if err != nil {
			return nil, err
		}

		if value != property.Value || source != "local" {
			continue
		}

		datasets = append(datasets, record.Name)
	}

	return datasets, nil
}

// datasetWithin reports whether name is a dataset (not a snapshot) at or below
// root, or anywhere when root is empty.
func datasetWithin(name, root string) bool {
	if strings.Contains(name, "@") {
		return false
	}

	return root == "" || name == root || strings.HasPrefix(name, root+"/")
}

// expandPropertySources replaces each zfsProperty source with one bare zfsPath
// source per matched dataset, keeping its compression.
func expandPropertySources(sources []config.Source, matches []PropertyMatches) []config.Source {
	byIndex := make(map[int][]string, len(matches))
	for _, match := range matches {
		byIndex[match.SourceIndex] = match.Datasets
	}

	expanded := make([]config.Source, 0, len(sources))

	for index, source := range sources {
		if source.ZFSProperty == nil {
			expanded = append(expanded, source)

			continue
		}

		for _, dataset := range byIndex[index] {
			expanded = append(expanded, config.Source{
				Compression: source.Compression,
				ZFSPath:     &config.ZFSPathSource{Name: dataset},
			})
		}
	}

	return expanded
}

// ResolveAndCheck validates and sizes every source on the data side (SPEC §4.3
// phase 1), rejecting any archive whose inflated estimate exceeds one tape's
// usable capacity, and returns the work list in config-source order.
func (a *ResolveDataActivities) ResolveAndCheck(ctx context.Context, input ResolveDataInput) ([]ResolvedArchive, error) {
	cfg := input.Config
	overhead := cfg.EffectiveFeasibilityOverhead()
	par2 := par2Fraction(cfg.Redundancy)
	// Use the same usable capacity Pack enforces (native less the LTFS reserve):
	// comparing against native admitted sources Pack then rejected only after
	// hours of staging (#224).
	capacity := usableCapacity(cfg.Library.TapeCapacityBytes)

	k8sByIndex := make(map[int]ResolvedArchive, len(input.K8sArchives))
	for _, archive := range input.K8sArchives {
		k8sByIndex[archive.SourceIndex] = archive
	}

	resolved := make([]ResolvedArchive, 0, len(cfg.Sources))

	var totalEstimate int64

	for index, source := range cfg.Sources {
		archive, err := a.resolveDataSource(ctx, index, source, k8sByIndex)
		if err != nil {
			return nil, err
		}

		logical, estimate, err := a.estimate(ctx, archive, overhead, par2)
		if err != nil {
			return nil, fmt.Errorf("estimate sources[%d]: %w", index, err)
		}

		archive.LogicalBytes = logical
		archive.EstimatedBytes = estimate

		if estimate > capacity {
			return nil, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"sources[%d] estimated size %d bytes exceeds one tape's usable capacity %d bytes "+
					"(native %d less the LTFS reserve Pack enforces)",
				index, estimate, capacity, cfg.Library.TapeCapacityBytes,
			), resolveInfeasibleErrorType, nil)
		}

		resolved = append(resolved, archive)
		totalEstimate += estimate

		slog.InfoContext(ctx, "resolve: sized source and confirmed it fits one tape",
			"sourceIndex", index, "label", archive.Label, "estimatedBytes", estimate, "tapeUsableCapacityBytes", capacity)
	}

	sliceSize := DeriveSliceSize(resolved)
	if err := checkPayloadBound(resolved, sliceSize); err != nil {
		return nil, err
	}

	slog.InfoContext(ctx, "resolve: work list validated; every archive fits one tape",
		"archives", len(resolved), "totalEstimatedBytes", totalEstimate, "tapeUsableCapacityBytes", capacity,
		"derivedSliceSizeBytes", sliceSize)

	return resolved, nil
}

const (
	// temporalPayloadLimitBytes is Temporal's default per-payload gRPC limit.
	// pkg/temporalclient does not override it; if it ever does, revisit this.
	temporalPayloadLimitBytes = 2 * 1024 * 1024

	// perStagedSliceBytes is a conservative serialized size of one StagedSlice
	// (measured ~200 B; headroom for long paths).
	perStagedSliceBytes = 256

	// payloadBoundBudgetBytes reserves half the limit for non-slice framing and
	// PAR2 file entries.
	payloadBoundBudgetBytes = temporalPayloadLimitBytes / 2
)

// Slice-size derivation constants (DeriveSliceSize, #324).
const (
	bytesPerGiB = 1 << 30

	// minSliceSizeBytes: below 1 GiB the per-slice PAR2 file and LTFS index entry
	// stop being negligible (#324 criterion 3).
	minSliceSizeBytes = bytesPerGiB

	// runSliceCountTarget bounds the whole-run slice count. The hard ceiling is
	// payloadBoundBudgetBytes/perStagedSliceBytes = 4096; 3500 leaves headroom for
	// per-archive ceiling rounding (one extra slice per archive).
	runSliceCountTarget = 3500

	// archiveSliceCountTarget keeps every archive under the 1,000-slice cap (#324)
	// and in the regime where PAR2 block sizing stays near ~2,000 blocks and the
	// Pack PAR2 reserve stays tight.
	archiveSliceCountTarget = 800
)

// DeriveSliceSize computes a run's slice size from its resolved archives (#324):
// the larger of totalEstimated/runSliceCountTarget and
// largestEstimated/archiveSliceCountTarget, rounded up to a whole GiB with a 1 GiB
// floor. It is pure, so Resolve retries and every consumer get the same value.
// EstimatedBytes over-estimates, so real slice counts never exceed these bounds.
func DeriveSliceSize(resolved []ResolvedArchive) int64 {
	var totalEstimated, largestEstimated int64

	for _, archive := range resolved {
		totalEstimated += archive.EstimatedBytes
		if archive.EstimatedBytes > largestEstimated {
			largestEstimated = archive.EstimatedBytes
		}
	}

	sliceSize := ceilDiv(totalEstimated, runSliceCountTarget)
	if archiveTerm := ceilDiv(largestEstimated, archiveSliceCountTarget); archiveTerm > sliceSize {
		sliceSize = archiveTerm
	}

	// Rounding up only lowers the slice count; the floor also covers an empty list.
	sliceSize = ceilDiv(sliceSize, bytesPerGiB) * bytesPerGiB
	if sliceSize < minSliceSizeBytes {
		sliceSize = minSliceSizeBytes
	}

	return sliceSize
}

// checkPayloadBound guards that the whole-run slice metadata fits Temporal's
// payload budget; it re-ships through Prepare, Pack, PAR2, Verify, Write and
// Report. The LTFS index is excluded because it is passed by path (#221).
// DeriveSliceSize satisfies this by construction, so a violation is a derivation
// bug, caught here rather than after 24 h of staging.
func checkPayloadBound(resolved []ResolvedArchive, sliceSizeBytes int64) error {
	if sliceSizeBytes <= 0 {
		return fmt.Errorf("internal: derived slice size must be positive, got %d bytes", sliceSizeBytes)
	}

	var totalEstimated, sliceCount int64

	for _, archive := range resolved {
		totalEstimated += archive.EstimatedBytes
		sliceCount += ceilDiv(archive.EstimatedBytes, sliceSizeBytes)
	}

	payloadBytes := sliceCount * perStagedSliceBytes
	if payloadBytes <= payloadBoundBudgetBytes {
		return nil
	}

	return fmt.Errorf(
		"internal: derived slice size %d bytes yields ~%d whole-run slices whose activity metadata "+
			"(~%d bytes) exceeds the Temporal payload budget (%d bytes) for estimated source size %d bytes "+
			"— DeriveSliceSize invariant violated",
		sliceSizeBytes, sliceCount, payloadBytes, int64(payloadBoundBudgetBytes), totalEstimated,
	)
}

// resolveDataSource verifies a control-resolved k8s archive, or validates and
// builds a raw ZFS one.
func (a *ResolveDataActivities) resolveDataSource(ctx context.Context, index int, source config.Source, k8sByIndex map[int]ResolvedArchive) (ResolvedArchive, error) {
	if source.K8s != nil {
		archive, ok := k8sByIndex[index]
		if !ok {
			return ResolvedArchive{}, fmt.Errorf("sources[%d]: k8s source was not resolved on the control worker", index)
		}

		for _, snapshot := range archive.Snapshots {
			// A PersistentVolumeClaim resolves to its live dataset, verified
			// before the run snapshots it.
			if snapshot.SnapshotName == "" {
				if err := k8ssnap.VerifyDataset(ctx, a.pool, snapshot.ZFSPath); err != nil {
					return ResolvedArchive{}, fmt.Errorf("verify sources[%d] PersistentVolumeClaim %s/%s dataset: %w",
						index, snapshot.Namespace, snapshot.PVC, err)
				}

				continue
			}

			verifySnapshot := k8ssnap.Snapshot{Dataset: snapshot.Dataset, SnapshotName: snapshot.SnapshotName}
			if err := k8ssnap.Verify(ctx, a.pool, verifySnapshot); err != nil {
				return ResolvedArchive{}, fmt.Errorf("verify sources[%d] snapshot %s: %w", index, snapshot.ZFSPath, err)
			}
		}

		return archive, nil
	}

	if source.ZFSPath == nil {
		return ResolvedArchive{}, fmt.Errorf("internal: sources[%d] is not a zfsPath source after zfsProperty expansion", index)
	}

	name := source.ZFSPath.Name
	if source.ZFSPath.Snapshot == config.SnapshotNewest {
		newest, err := a.newestSnapshot(ctx, name, source.ZFSPath.Prefix)
		if err != nil {
			return ResolvedArchive{}, fmt.Errorf("resolve raw zfs sources[%d] %q newest snapshot: %w", index, name, err)
		}

		name = newest
		slog.InfoContext(ctx, "resolve: selected newest snapshot", "sourceIndex", index, "snapshot", name)
	}

	// Existence check: zfs exits non-zero for an absent dataset or snapshot.
	if _, err := a.pool.UserProperties(ctx, name); err != nil {
		return ResolvedArchive{}, fmt.Errorf("validate raw zfs sources[%d] %q: %w", index, name, err)
	}

	return ResolvedArchive{
		SourceIndex: index,
		Label:       sourceLabel(source),
		Compression: compressionEnabled(source),
		Snapshots:   []ResolvedSnapshot{{ZFSPath: name}},
	}, nil
}

// newestSnapshot returns "dataset@snap" for the newest (creation, then createtxg)
// snapshot whose name starts with prefix.
func (a *ResolveDataActivities) newestSnapshot(ctx context.Context, dataset, prefix string) (string, error) {
	snapshots, err := a.pool.Snapshots(ctx, dataset)
	if err != nil {
		return "", err
	}

	var newest *zfs.SnapshotInfo

	for index := range snapshots {
		snapshot := &snapshots[index]
		if !strings.HasPrefix(snapshot.Name, prefix) {
			continue
		}

		if newest == nil || snapshot.Creation > newest.Creation ||
			(snapshot.Creation == newest.Creation && snapshot.CreateTXG > newest.CreateTXG) {
			newest = snapshot
		}
	}

	switch {
	case newest == nil && prefix != "":
		return "", fmt.Errorf("no snapshot name starts with prefix %q", prefix)
	case newest == nil:
		return "", errors.New("dataset has no snapshots")
	}

	return dataset + "@" + newest.Name, nil
}

// estimate returns the summed logicalreferenced (Prepare's progress denominator)
// and the inflated feasibility estimate.
func (a *ResolveDataActivities) estimate(ctx context.Context, archive ResolvedArchive, overhead, par2 float64) (logical, estimate int64, err error) {
	for _, snapshot := range archive.Snapshots {
		referenced, err := a.pool.LogicalReferenced(ctx, snapshot.ZFSPath)
		if err != nil {
			return 0, 0, fmt.Errorf("read logicalreferenced for %s: %w", snapshot.ZFSPath, err)
		}

		logical += referenced
	}

	return logical, feasibilityEstimate(logical, overhead, par2), nil
}

// compressionEnabled defaults to true when unset (SPEC §8).
func compressionEnabled(source config.Source) bool {
	if source.Compression != nil {
		return *source.Compression
	}

	return true
}

// par2Fraction is the PAR2 fraction used in the feasibility estimate. Fill mode
// uses its floor: fill only adds parity into otherwise-wasted space, so it cannot
// make a fitting archive stop fitting.
func par2Fraction(redundancy config.Redundancy) float64 {
	switch {
	case redundancy.TargetPercentage != nil:
		return *redundancy.TargetPercentage / 100
	case redundancy.FillToCapacity != nil:
		return redundancy.FillToCapacity.Floor / 100
	default:
		return 0
	}
}

// feasibilityEstimate rounds up so the pre-check never under-estimates.
func feasibilityEstimate(logical int64, overhead, par2 float64) int64 {
	return int64(math.Ceil(float64(logical) * overhead * (1 + par2)))
}

// resolvePhase expands zfsProperty sources, runs control-side k8s resolution
// then the data-side checks, and stores the work list with every live dataset
// pointed at the run's own snapshot. A failure aborts the run before any data
// is staged.
func resolvePhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: resolveDataTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: resolveMaxAttempts},
	})

	var data *ResolveDataActivities

	// The matches are recorded in history, so the expansion replays identically.
	if slices.ContainsFunc(cfg.Sources, func(source config.Source) bool { return source.ZFSProperty != nil }) {
		var matches []PropertyMatches
		if err := workflow.ExecuteActivity(dataCtx, data.ResolvePropertySources, cfg).Get(dataCtx, &matches); err != nil {
			return err
		}

		cfg.Sources = expandPropertySources(cfg.Sources, matches)
		state.sources = cfg.Sources
	}

	controlCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: resolveControlTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: resolveMaxAttempts},
	})

	var control *ResolveControlActivities

	if slices.ContainsFunc(cfg.Sources, isClaim) {
		var matches []ClaimMatches
		if slices.ContainsFunc(cfg.Sources, isClaimSelector) {
			if err := workflow.ExecuteActivity(controlCtx, control.ListClaimSources, cfg).Get(controlCtx, &matches); err != nil {
				return err
			}
		}

		expanded, err := expandClaimSources(cfg.Sources, matches)
		if err != nil {
			return temporal.NewNonRetryableApplicationError(err.Error(), resolveLabelCollisionErrorType, nil)
		}

		cfg.Sources = expanded
		state.sources = cfg.Sources
	}

	var k8sArchives []ResolvedArchive
	if err := workflow.ExecuteActivity(controlCtx, control.ResolveK8sSources, cfg).Get(controlCtx, &k8sArchives); err != nil {
		return err
	}

	input := ResolveDataInput{Config: cfg, K8sArchives: k8sArchives}

	var resolved []ResolvedArchive
	if err := workflow.ExecuteActivity(dataCtx, data.ResolveAndCheck, input).Get(dataCtx, &resolved); err != nil {
		return err
	}

	planRunScopedSnapshots(resolved, workflow.GetInfo(ctx).WorkflowExecution.RunID)

	state.resolved = resolved
	// Derived once here so Prepare and Report share one deterministic value (#324).
	state.sliceSizeBytes = DeriveSliceSize(resolved)

	return nil
}
