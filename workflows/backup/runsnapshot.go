package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// Run-scoped snapshots (SPEC §4.3): a source that resolves to a live dataset is
// archived from a snapshot the run takes after Resolve and destroys on every
// exit path. Ownership is proven by name AND by the RunIDProperty set at
// creation, so cleanup never destroys a snapshot tape-archiver did not create.

const (
	runSnapshotPrefix = "tape-archiver-"

	// RunIDProperty is set on every run-scoped snapshot to the creating run's id.
	RunIDProperty = "tape-archiver:run-id"

	runSnapshotTimeout     = 10 * time.Minute
	runSnapshotMaxAttempts = 3

	// runSnapshotConflictErrorType marks a run-scoped name already taken by a
	// snapshot this run did not create.
	runSnapshotConflictErrorType = "run-snapshot-conflict"
)

// RunSnapshotName returns the run-scoped snapshot's short name for a run id,
// mapping characters ZFS rejects in a snapshot name to "-".
func RunSnapshotName(runID string) string {
	return runSnapshotPrefix + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == ':':
			return r
		default:
			return '-'
		}
	}, runID)
}

// planRunScopedSnapshots points every bare-dataset snapshot in resolved at the
// run's own snapshot of that dataset, and returns the distinct snapshot paths.
func planRunScopedSnapshots(resolved []ResolvedArchive, runID string) []string {
	name := RunSnapshotName(runID)
	seen := make(map[string]struct{})

	var paths []string

	for archiveIndex := range resolved {
		for snapshotIndex := range resolved[archiveIndex].Snapshots {
			snapshot := &resolved[archiveIndex].Snapshots[snapshotIndex]
			if snapshot.Dataset != "" || strings.Contains(snapshot.ZFSPath, "@") {
				continue
			}

			snapshot.Dataset = snapshot.ZFSPath
			snapshot.SnapshotName = name
			snapshot.ZFSPath = snapshot.Dataset + "@" + name
			snapshot.RunScoped = true

			if _, ok := seen[snapshot.ZFSPath]; !ok {
				seen[snapshot.ZFSPath] = struct{}{}
				paths = append(paths, snapshot.ZFSPath)
			}
		}
	}

	return paths
}

// runScopedSnapshotPaths returns the distinct run-scoped snapshot paths.
func runScopedSnapshotPaths(state *runState) []string {
	seen := make(map[string]struct{})

	var paths []string

	for _, archive := range state.resolved {
		for _, snapshot := range archive.Snapshots {
			if _, ok := seen[snapshot.ZFSPath]; ok || !snapshot.RunScoped {
				continue
			}

			seen[snapshot.ZFSPath] = struct{}{}
			paths = append(paths, snapshot.ZFSPath)
		}
	}

	return paths
}

// snapshotManager is the test seam over the zfs snapshot lifecycle calls.
type snapshotManager interface {
	CreateSnapshots(ctx context.Context, properties map[string]string, snapshots []string) error
	DestroySnapshot(ctx context.Context, snapshot string) error
	Creation(ctx context.Context, name string) (int64, error)
	UserProperty(ctx context.Context, name, property string) (string, error)
	PropertySource(ctx context.Context, name, property string) (string, error)
	LocalProperty(ctx context.Context, property, datasetType, root string) ([]zfs.PropertyRecord, error)
	Holds(ctx context.Context, snapshot string) ([]string, error)
	Release(ctx context.Context, tag, snapshot string) error
}

type zfsSnapshotManager struct{}

func (zfsSnapshotManager) CreateSnapshots(ctx context.Context, properties map[string]string, snapshots []string) error {
	return zfs.CreateSnapshots(ctx, properties, snapshots)
}

func (zfsSnapshotManager) DestroySnapshot(ctx context.Context, snapshot string) error {
	return zfs.DestroySnapshot(ctx, snapshot)
}

func (zfsSnapshotManager) Creation(ctx context.Context, name string) (int64, error) {
	return zfs.Creation(ctx, name)
}

func (zfsSnapshotManager) UserProperty(ctx context.Context, name, property string) (string, error) {
	return zfs.UserProperty(ctx, name, property)
}

func (zfsSnapshotManager) PropertySource(ctx context.Context, name, property string) (string, error) {
	return zfs.PropertySource(ctx, name, property)
}

func (zfsSnapshotManager) LocalProperty(ctx context.Context, property, datasetType, root string) ([]zfs.PropertyRecord, error) {
	return zfs.LocalProperty(ctx, property, datasetType, root)
}

func (zfsSnapshotManager) Holds(ctx context.Context, snapshot string) ([]string, error) {
	return zfs.Holds(ctx, snapshot)
}

func (zfsSnapshotManager) Release(ctx context.Context, tag, snapshot string) error {
	return zfs.Release(ctx, tag, snapshot)
}

// RunSnapshotActivities hosts the data-side run-scoped snapshot activities.
type RunSnapshotActivities struct {
	zfs snapshotManager
}

func newRunSnapshotActivities() *RunSnapshotActivities {
	return &RunSnapshotActivities{zfs: zfsSnapshotManager{}}
}

// RunSnapshotInput names the run and its run-scoped snapshot paths.
type RunSnapshotInput struct {
	RunID     string
	Snapshots []string
}

// CreatedSnapshot is a run-scoped snapshot and its creation time (Unix seconds).
type CreatedSnapshot struct {
	ZFSPath  string
	Creation int64
}

// CreateRunSnapshots first destroys snapshots leaked by earlier runs, then takes
// the run's snapshots, one atomic `zfs snapshot` per pool. A retry keeps any
// already taken by this run.
func (a *RunSnapshotActivities) CreateRunSnapshots(ctx context.Context, input RunSnapshotInput) ([]CreatedSnapshot, error) {
	a.removeLeaked(ctx, input.RunID)

	byPool := make(map[string][]string)

	for _, snapshot := range input.Snapshots {
		owner, err := a.zfs.UserProperty(ctx, snapshot, RunIDProperty)

		switch {
		case err != nil && strings.Contains(err.Error(), zfs.ErrNotExist.Error()):
			pool, _, _ := strings.Cut(snapshot, "/")
			pool, _, _ = strings.Cut(pool, "@")
			byPool[pool] = append(byPool[pool], snapshot)
		case err != nil:
			return nil, fmt.Errorf("check run snapshot %s: %w", snapshot, err)
		case owner != input.RunID:
			return nil, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"snapshot %s already exists and was not created by this run (%s=%q)", snapshot, RunIDProperty, owner),
				runSnapshotConflictErrorType, nil)
		}
	}

	pools := make([]string, 0, len(byPool))
	for pool := range byPool {
		pools = append(pools, pool)
	}

	sort.Strings(pools)

	for _, pool := range pools {
		if err := a.zfs.CreateSnapshots(ctx, map[string]string{RunIDProperty: input.RunID}, byPool[pool]); err != nil {
			return nil, fmt.Errorf("take run-scoped snapshots in pool %s: %w", pool, err)
		}
	}

	created := make([]CreatedSnapshot, 0, len(input.Snapshots))

	for _, snapshot := range input.Snapshots {
		creation, err := a.zfs.Creation(ctx, snapshot)
		if err != nil {
			return nil, fmt.Errorf("read creation of run snapshot %s: %w", snapshot, err)
		}

		created = append(created, CreatedSnapshot{ZFSPath: snapshot, Creation: creation})
	}

	slog.InfoContext(ctx, "snapshot: took the run's snapshots of live datasets",
		"snapshots", input.Snapshots, "pools", pools)

	return created, nil
}

// DestroyRunSnapshots destroys the run's snapshots. A missing snapshot is a
// no-op; one not carrying this run's id is left alone.
func (a *RunSnapshotActivities) DestroyRunSnapshots(ctx context.Context, input RunSnapshotInput) error {
	var errs []error

	for _, snapshot := range input.Snapshots {
		owner, err := a.zfs.UserProperty(ctx, snapshot, RunIDProperty)

		switch {
		case err != nil && strings.Contains(err.Error(), zfs.ErrNotExist.Error()):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("check run snapshot %s: %w", snapshot, err))

			continue
		case owner != input.RunID:
			slog.WarnContext(ctx, "snapshot: not destroying a snapshot this run did not create",
				"snapshot", snapshot, "runIdProperty", owner)

			continue
		}

		if err := a.zfs.DestroySnapshot(ctx, snapshot); err != nil {
			errs = append(errs, fmt.Errorf("destroy run snapshot %s: %w", snapshot, err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return err
	}

	slog.InfoContext(ctx, "snapshot: destroyed the run's snapshots", "snapshots", len(input.Snapshots))

	return nil
}

// removeLeaked destroys run-scoped snapshots of any other run. Only one run is
// ever active (fixed workflow id), so they are leaks. Best effort: a failure is
// logged and the next run tries again.
func (a *RunSnapshotActivities) removeLeaked(ctx context.Context, runID string) {
	records, err := a.zfs.LocalProperty(ctx, RunIDProperty, "snapshot", "")
	if err != nil {
		slog.WarnContext(ctx, "snapshot: could not list leaked run snapshots", "error", err)

		return
	}

	for _, record := range records {
		if record.Value == runID || !a.isLeakedRunSnapshot(ctx, record) {
			continue
		}

		if err := a.destroyLeaked(ctx, record.Name); err != nil {
			slog.WarnContext(ctx, "snapshot: could not remove a leaked run snapshot", "snapshot", record.Name, "error", err)

			continue
		}

		slog.InfoContext(ctx, "snapshot: removed a run snapshot leaked by an earlier run",
			"snapshot", record.Name, "leakedByRunId", record.Value)
	}
}

// isLeakedRunSnapshot confirms a listed record with single-property reads (a
// value with an embedded newline can fabricate list records) and requires the
// name to be the run snapshot name of the recorded run id.
func (a *RunSnapshotActivities) isLeakedRunSnapshot(ctx context.Context, record zfs.PropertyRecord) bool {
	_, short, found := strings.Cut(record.Name, "@")
	if !found || record.Value == "" || record.Value == "-" || short != RunSnapshotName(record.Value) {
		return false
	}

	value, err := a.zfs.UserProperty(ctx, record.Name, RunIDProperty)
	if err != nil || value != record.Value {
		return false
	}

	source, err := a.zfs.PropertySource(ctx, record.Name, RunIDProperty)

	return err == nil && source == "local"
}

// destroyLeaked releases tape-archiver's own holds on snapshot, then destroys it.
func (a *RunSnapshotActivities) destroyLeaked(ctx context.Context, snapshot string) error {
	tags, err := a.zfs.Holds(ctx, snapshot)
	if err != nil {
		return err
	}

	for _, tag := range tags {
		if !strings.HasPrefix(tag, holdTagPrefix) {
			continue
		}

		if err := a.zfs.Release(ctx, tag, snapshot); err != nil {
			return err
		}
	}

	return a.zfs.DestroySnapshot(ctx, snapshot)
}

// createRunSnapshots takes the run's snapshots and records their creation
// times for the report. A failure fails the run before staging.
func createRunSnapshots(ctx workflow.Context, state *runState) error {
	snapshots := runScopedSnapshotPaths(state)
	if len(snapshots) == 0 {
		return nil
	}

	input := RunSnapshotInput{RunID: workflow.GetInfo(ctx).WorkflowExecution.RunID, Snapshots: snapshots}

	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: runSnapshotTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: runSnapshotMaxAttempts},
	})

	var activities *RunSnapshotActivities

	var created []CreatedSnapshot
	if err := workflow.ExecuteActivity(actx, activities.CreateRunSnapshots, input).Get(actx, &created); err != nil {
		return err
	}

	creation := make(map[string]time.Time, len(created))
	for _, snapshot := range created {
		creation[snapshot.ZFSPath] = time.Unix(snapshot.Creation, 0).UTC()
	}

	for archiveIndex := range state.resolved {
		for snapshotIndex := range state.resolved[archiveIndex].Snapshots {
			snapshot := &state.resolved[archiveIndex].Snapshots[snapshotIndex]
			if snapshot.RunScoped {
				snapshot.CreatedAt = creation[snapshot.ZFSPath]
			}
		}
	}

	return nil
}

// destroyRunSnapshots destroys the run's snapshots on a disconnected context,
// after the holds are released, with the release retry window. A failure is
// returned for the caller to surface; the snapshot is then leaked until the
// next run or a manual cleanup (docs/maintenance.md).
func destroyRunSnapshots(ctx workflow.Context, state *runState) error {
	snapshots := runScopedSnapshotPaths(state)
	if len(snapshots) == 0 {
		return nil
	}

	input := RunSnapshotInput{RunID: workflow.GetInfo(ctx).WorkflowExecution.RunID, Snapshots: snapshots}

	disconnected, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()

	actx := workflow.WithActivityOptions(disconnected, workflow.ActivityOptions{
		TaskQueue:              DataTaskQueue,
		StartToCloseTimeout:    runSnapshotTimeout,
		ScheduleToCloseTimeout: releaseWindow,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    releaseInitialInterval,
			BackoffCoefficient: 2,
			MaximumInterval:    releaseMaxInterval,
			MaximumAttempts:    releaseMaxAttempts,
		},
	})

	var activities *RunSnapshotActivities
	if err := workflow.ExecuteActivity(actx, activities.DestroyRunSnapshots, input).Get(actx, nil); err != nil {
		workflow.GetLogger(ctx).Error("failed to destroy run-scoped snapshots", "snapshots", snapshots, "error", err)

		return fmt.Errorf("destroy run-scoped snapshots %v (leaked; the next run or docs/maintenance.md removes them): %w",
			snapshots, err)
	}

	return nil
}
