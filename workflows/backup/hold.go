package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// The source-snapshot hold (SPEC §4.3) pins every resolved snapshot with a
// `zfs hold` from Resolve until run exit, so external pruning cannot destroy it
// mid-run. The tag derives from the run id: no cross-run state (SPEC §4.2), and a
// leaked hold never collides with a future run's.

const (
	holdTagPrefix = "tape-archiver-hold-"

	holdTimeout = 5 * time.Minute

	// holdMaxAttempts bounds hold and release retries: unbounded, a vanished
	// snapshot wedges the run, and the release defer blocks the failure alert
	// and staging cleanup behind it (#387).
	holdMaxAttempts = 3

	// snapshotNotFoundErrorType marks a resolved snapshot destroyed before its hold.
	snapshotNotFoundErrorType = "snapshot-not-found"
)

// HoldTag returns the run-scoped ZFS hold tag for a Temporal run id.
func HoldTag(runID string) string {
	return holdTagPrefix + runID
}

// snapshotHolder is the test seam over the zfs hold/release functions.
type snapshotHolder interface {
	Hold(ctx context.Context, tag, snapshot string) error
	Release(ctx context.Context, tag, snapshot string) error
}

// zfsHolder is the production snapshotHolder over the zfs CLI.
type zfsHolder struct{}

func (zfsHolder) Hold(ctx context.Context, tag, snapshot string) error {
	return zfs.Hold(ctx, tag, snapshot)
}

func (zfsHolder) Release(ctx context.Context, tag, snapshot string) error {
	return zfs.Release(ctx, tag, snapshot)
}

// HoldActivities hosts the data-side snapshot hold and release activities.
type HoldActivities struct {
	holder snapshotHolder
}

func newHoldActivities() *HoldActivities {
	return &HoldActivities{holder: zfsHolder{}}
}

// HoldInput is the payload for both hold activities.
type HoldInput struct {
	Tag string
	// Snapshots are absolute ZFS snapshot paths.
	Snapshots []string
}

// HoldSnapshots places the run's hold on every snapshot; re-holding is a no-op.
func (a *HoldActivities) HoldSnapshots(ctx context.Context, input HoldInput) error {
	for _, snapshot := range input.Snapshots {
		if err := a.holder.Hold(ctx, input.Tag, snapshot); err != nil {
			err = fmt.Errorf("hold snapshot %s with tag %s: %w", snapshot, input.Tag, err)
			if errors.Is(err, zfs.ErrNotExist) {
				return temporal.NewNonRetryableApplicationError(err.Error(), snapshotNotFoundErrorType, err)
			}

			return err
		}
	}

	slog.InfoContext(ctx, "hold: pinned source snapshots against pruning for the run",
		"snapshots", len(input.Snapshots), "tag", input.Tag)

	return nil
}

// ReleaseSnapshots removes the run's hold from every snapshot; an absent hold is
// a no-op. One failure does not stop the others from being released.
func (a *HoldActivities) ReleaseSnapshots(ctx context.Context, input HoldInput) error {
	var errs []error

	for _, snapshot := range input.Snapshots {
		if err := a.holder.Release(ctx, input.Tag, snapshot); err != nil {
			errs = append(errs, fmt.Errorf("release snapshot %s tag %s: %w", snapshot, input.Tag, err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return err
	}

	slog.InfoContext(ctx, "hold: released the run's holds on source snapshots",
		"snapshots", len(input.Snapshots), "tag", input.Tag)

	return nil
}

// holdSnapshotPaths returns the distinct snapshot paths to hold, skipping bare
// datasets (no "@") that have no snapshot.
func holdSnapshotPaths(state *runState) []string {
	seen := make(map[string]struct{})

	var paths []string

	for _, archive := range state.resolved {
		for _, snapshot := range archive.Snapshots {
			if !strings.Contains(snapshot.ZFSPath, "@") {
				continue
			}

			if _, ok := seen[snapshot.ZFSPath]; ok {
				continue
			}

			seen[snapshot.ZFSPath] = struct{}{}
			paths = append(paths, snapshot.ZFSPath)
		}
	}

	return paths
}

// holdSnapshots pins the resolved snapshots; a failure fails the run before any
// staging begins.
func holdSnapshots(ctx workflow.Context, state *runState) error {
	snapshots := holdSnapshotPaths(state)
	if len(snapshots) == 0 {
		return nil
	}

	input := HoldInput{
		Tag:       HoldTag(workflow.GetInfo(ctx).WorkflowExecution.RunID),
		Snapshots: snapshots,
	}

	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: holdTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: holdMaxAttempts},
	})

	var activities *HoldActivities

	return workflow.ExecuteActivity(actx, activities.HoldSnapshots, input).Get(actx, nil)
}

// releaseSnapshots releases the run's holds on a disconnected context, so it
// also fires on cancellation. It logs but never returns an error, so a leaked
// hold (manually clearable, docs/maintenance.md) cannot mask the run's outcome.
func releaseSnapshots(ctx workflow.Context, state *runState) {
	snapshots := holdSnapshotPaths(state)
	if len(snapshots) == 0 {
		return
	}

	input := HoldInput{
		Tag:       HoldTag(workflow.GetInfo(ctx).WorkflowExecution.RunID),
		Snapshots: snapshots,
	}

	disconnected, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()

	actx := workflow.WithActivityOptions(disconnected, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: holdTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: holdMaxAttempts},
	})

	var activities *HoldActivities
	if err := workflow.ExecuteActivity(actx, activities.ReleaseSnapshots, input).Get(actx, nil); err != nil {
		workflow.GetLogger(ctx).Error("failed to release source snapshot holds",
			"tag", input.Tag,
			"error", err,
		)
	}
}
