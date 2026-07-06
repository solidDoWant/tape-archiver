package backup

import (
	"context"
	"fmt"
	"math"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/k8ssnap"
	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// The Resolve phase (SPEC §4.3 phase 1) expands the run config into a concrete
// work list and runs the feasibility pre-check, split across two activities by
// the SPEC §16 ownership rule:
//
//   - ResolveK8sSources runs on the control worker, where the Kubernetes API is
//     reachable: it turns each k8s VolumeSnapshot / label-selector group into the
//     ZFS snapshot path(s) it maps to.
//   - ResolveAndCheck runs on the data worker, where the ZFS pool is reachable:
//     it validates raw ZFS sources, verifies each resolved k8s snapshot exists and
//     is democratic-csi-managed, sizes every archive, and rejects any single
//     archive that cannot fit on one tape — all before any data is staged.
//
// Both produce ResolvedArchive values; the resolved work list is the only thing
// this phase leaves behind, in runState.resolved.

const (
	// resolveControlTimeout bounds the control-side k8s resolution. It is a few
	// Kubernetes API reads per source, so minutes are ample.
	resolveControlTimeout = 5 * time.Minute
	// resolveDataTimeout bounds the data-side validation, verification, and
	// feasibility sizing. Each is a cheap `zfs get`, but a run may name many
	// sources, so the bound is generous.
	resolveDataTimeout = 10 * time.Minute
)

// snapshotResolver resolves k8s VolumeSnapshot references to democratic-csi ZFS
// snapshot paths. It is the seam *k8ssnap.Resolver satisfies, injected so the
// control activity is unit-testable without a cluster.
type snapshotResolver interface {
	Resolve(ctx context.Context, ref k8ssnap.Ref) (k8ssnap.Snapshot, error)
	ResolveGroup(ctx context.Context, ref k8ssnap.Ref) (k8ssnap.Group, error)
}

// poolInspector reads ZFS properties from the pool. It is the seam the zfs
// package functions are wrapped behind, injected so the data activity is
// unit-testable without a pool. Its UserProperties method also satisfies
// k8ssnap.PropertyReader, so it doubles as the reader for ownership verification.
type poolInspector interface {
	UserProperties(ctx context.Context, dataset string) (map[string]string, error)
	LogicalReferenced(ctx context.Context, dataset string) (int64, error)
}

// zfsPool is the production poolInspector: a thin adapter over the package-level
// zfs functions, which shell out to the zfs CLI on the data worker.
type zfsPool struct{}

func (zfsPool) UserProperties(ctx context.Context, dataset string) (map[string]string, error) {
	return zfs.UserProperties(ctx, dataset)
}

func (zfsPool) LogicalReferenced(ctx context.Context, dataset string) (int64, error) {
	return zfs.LogicalReferenced(ctx, dataset)
}

// ResolveControlActivities hosts the control-side Resolve activity. It builds the
// k8s resolver lazily on first use, so a run that names only raw ZFS sources
// needs no Kubernetes access at all.
type ResolveControlActivities struct {
	// newResolver constructs the k8s resolver. It is a factory rather than a
	// stored resolver so the (possibly failing) Kubernetes client is built only
	// when a run actually has k8s sources, and so tests can inject a fake.
	newResolver func() (snapshotResolver, error)
}

// newResolveControlActivities returns the production control-side Resolve
// activity, building its resolver from the ambient Kubernetes config and
// democratic-csi's datasetParentName.
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

// ResolveK8sSources resolves every k8s source in cfg to the ZFS snapshot(s) it
// maps to (SPEC §4.3 phase 1, control side). A source with a Name resolves to a
// single snapshot; one with a LabelSelector resolves to a group, all packed into
// one archive (SPEC §5). Raw ZFS sources are skipped here — they are the data
// worker's job. A single resolution failure fails the run before any data is
// staged.
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
	}

	return resolved, nil
}

// resolveK8sSource resolves one k8s source to a ResolvedArchive, dispatching on
// whether it names a single VolumeSnapshot or a label-selector group.
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

// snapshotFromK8s maps a k8ssnap.Snapshot to the run-state ResolvedSnapshot,
// carrying both the ZFS location and the k8s provenance.
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

// ResolveDataInput is the payload for the data-side Resolve activity: the run
// config plus the k8s archives the control worker already resolved (keyed back to
// their source by SourceIndex).
type ResolveDataInput struct {
	Config      config.Config
	K8sArchives []ResolvedArchive
}

// ResolveDataActivities hosts the data-side Resolve activity, which runs where the
// ZFS pool is reachable.
type ResolveDataActivities struct {
	pool poolInspector
}

// newResolveDataActivities returns the production data-side Resolve activity,
// reading the pool through the zfs CLI. The tape capacity it checks feasibility
// against comes from the run config, per source.
func newResolveDataActivities() *ResolveDataActivities {
	return &ResolveDataActivities{pool: zfsPool{}}
}

// ResolveAndCheck completes the work list on the data side (SPEC §4.3 phase 1):
// it validates raw ZFS sources exist, verifies each control-resolved k8s snapshot
// exists and is democratic-csi-managed, then sizes every archive and rejects any
// whose estimate exceeds one tape's capacity. The estimate inflates
// logicalreferenced by the configured overhead factor and PAR2 % purely for this
// pre-check; it is never the authoritative plan (that is the measured Prepare
// size). It returns the full work list in config-source order.
func (a *ResolveDataActivities) ResolveAndCheck(ctx context.Context, input ResolveDataInput) ([]ResolvedArchive, error) {
	cfg := input.Config
	overhead := cfg.EffectiveFeasibilityOverhead()
	par2 := par2Fraction(cfg.Redundancy)
	capacity := cfg.Library.TapeCapacityBytes

	k8sByIndex := make(map[int]ResolvedArchive, len(input.K8sArchives))
	for _, archive := range input.K8sArchives {
		k8sByIndex[archive.SourceIndex] = archive
	}

	resolved := make([]ResolvedArchive, 0, len(cfg.Sources))

	for index, source := range cfg.Sources {
		archive, err := a.resolveDataSource(ctx, index, source, k8sByIndex)
		if err != nil {
			return nil, err
		}

		estimate, err := a.estimate(ctx, archive, overhead, par2)
		if err != nil {
			return nil, fmt.Errorf("estimate sources[%d]: %w", index, err)
		}

		archive.EstimatedBytes = estimate

		if estimate > capacity {
			return nil, fmt.Errorf(
				"sources[%d] estimated size %d bytes exceeds one tape's capacity %d bytes",
				index, estimate, capacity,
			)
		}

		resolved = append(resolved, archive)
	}

	return resolved, nil
}

// resolveDataSource produces the data-side ResolvedArchive for one source: it
// verifies a control-resolved k8s archive, or validates and builds a raw ZFS one.
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

	// Reading a raw source's user properties doubles as an existence check: zfs
	// exits non-zero for an absent dataset or snapshot (SPEC §4.3 phase 1).
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

// estimate returns the feasibility estimate for an archive: the summed
// logicalreferenced of its snapshots inflated by the overhead factor and PAR2 %.
func (a *ResolveDataActivities) estimate(ctx context.Context, archive ResolvedArchive, overhead, par2 float64) (int64, error) {
	var logical int64

	for _, snapshot := range archive.Snapshots {
		referenced, err := a.pool.LogicalReferenced(ctx, snapshot.ZFSPath)
		if err != nil {
			return 0, fmt.Errorf("read logicalreferenced for %s: %w", snapshot.ZFSPath, err)
		}

		logical += referenced
	}

	return feasibilityEstimate(logical, overhead, par2), nil
}

// compressionEnabled reports whether the Prepare phase should zstd-compress a
// source's archive, applying the default-on rule when the source leaves it unset
// (SPEC §8).
func compressionEnabled(source config.Source) bool {
	if source.Compression != nil {
		return *source.Compression
	}

	return true
}

// par2Fraction is the PAR2 redundancy used in the feasibility estimate, as a
// fraction of archive size. Fixed mode uses its target percentage; fill-to-
// capacity mode uses its floor — the minimum footprint, since fill only adds
// parity into otherwise-wasted tape space and so cannot make an archive that
// fits at the floor stop fitting.
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

// feasibilityEstimate inflates a logical size by the overhead factor and PAR2
// fraction, rounding up so the pre-check never under-estimates (SPEC §4.3 phase
// 1).
func feasibilityEstimate(logical int64, overhead, par2 float64) int64 {
	return int64(math.Ceil(float64(logical) * overhead * (1 + par2)))
}

// resolvePhase orchestrates the Resolve phase: control-side k8s resolution, then
// data-side validation, verification, and the feasibility pre-check. It stores
// the resolved work list in runState for the Prepare phase. A failure in either
// activity aborts the run here, before any data is staged.
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

	return nil
}
