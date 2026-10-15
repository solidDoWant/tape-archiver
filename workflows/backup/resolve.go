package backup

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

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

// ResolveControlActivities hosts the control-side Resolve activity.
type ResolveControlActivities struct {
	// newResolver is a factory so the Kubernetes client is built only when a run
	// has k8s sources; raw-ZFS-only runs need no cluster access.
	newResolver func() (snapshotResolver, error)
}

func newResolveControlActivities(datasetParent string) *ResolveControlActivities {
	return &ResolveControlActivities{
		newResolver: func() (snapshotResolver, error) {
			client, err := k8ssnap.NewClient()
			if err != nil {
				return nil, err
			}

			return k8ssnap.NewResolver(client, datasetParent), nil
		},
	}
}

// ResolveK8sSources resolves every k8s source to its ZFS snapshot(s); a
// LabelSelector source becomes one grouped archive (SPEC §5). Raw ZFS sources are
// left to the data worker.
func (a *ResolveControlActivities) ResolveK8sSources(ctx context.Context, cfg config.Config) ([]ResolvedArchive, error) {
	var resolver snapshotResolver

	var resolved []ResolvedArchive

	for index, source := range cfg.Sources {
		if source.K8s == nil {
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
	ref := k8ssnap.Ref{Namespace: k8s.Namespace, Name: k8s.Name, LabelSelector: k8s.LabelSelector}
	archive := ResolvedArchive{SourceIndex: index, Label: sourceLabel(source), Compression: compressionEnabled(source)}

	if k8s.LabelSelector != "" {
		group, err := resolver.ResolveGroup(ctx, ref)
		if err != nil {
			return ResolvedArchive{}, fmt.Errorf("resolve sources[%d] k8s group: %w", index, err)
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

// ResolveDataActivities hosts the data-side Resolve activity.
type ResolveDataActivities struct {
	pool poolInspector
}

func newResolveDataActivities() *ResolveDataActivities {
	return &ResolveDataActivities{pool: zfsPool{}}
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
			return nil, fmt.Errorf(
				"sources[%d] estimated size %d bytes exceeds one tape's usable capacity %d bytes "+
					"(native %d less the LTFS reserve Pack enforces)",
				index, estimate, capacity, cfg.Library.TapeCapacityBytes,
			)
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
			verifySnapshot := k8ssnap.Snapshot{Dataset: snapshot.Dataset, SnapshotName: snapshot.SnapshotName}
			if err := k8ssnap.Verify(ctx, a.pool, verifySnapshot); err != nil {
				return ResolvedArchive{}, fmt.Errorf("verify sources[%d] snapshot %s: %w", index, snapshot.ZFSPath, err)
			}
		}

		return archive, nil
	}

	// Existence check: zfs exits non-zero for an absent dataset or snapshot.
	name := source.ZFSPath.Name
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

// resolvePhase runs control-side k8s resolution then the data-side checks, and
// stores the work list. A failure aborts the run before any data is staged.
func resolvePhase(ctx workflow.Context, cfg config.Config, state *runState) error {
	controlCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueue,
		StartToCloseTimeout: resolveControlTimeout,
	})

	var control *ResolveControlActivities

	var k8sArchives []ResolvedArchive
	if err := workflow.ExecuteActivity(controlCtx, control.ResolveK8sSources, cfg).Get(controlCtx, &k8sArchives); err != nil {
		return err
	}

	dataCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: resolveDataTimeout,
	})

	var data *ResolveDataActivities

	input := ResolveDataInput{Config: cfg, K8sArchives: k8sArchives}

	var resolved []ResolvedArchive
	if err := workflow.ExecuteActivity(dataCtx, data.ResolveAndCheck, input).Get(dataCtx, &resolved); err != nil {
		return err
	}

	state.resolved = resolved
	// Derived once here so Prepare and Report share one deterministic value (#324).
	state.sliceSizeBytes = DeriveSliceSize(resolved)

	return nil
}
