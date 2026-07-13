package backup

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// The source-snapshot hold (SPEC §4.3) brackets the Prepare-onward window: right
// after Resolve produces the concrete work list, the run places a `zfs hold` on
// every resolved source snapshot, and releases every hold on every exit path
// (success, failure, cancellation). This pins the snapshots against external
// pruning for the run's duration — an external `zfs destroy` of a held snapshot
// is refused while the run owns it. No cross-run state is introduced: the hold
// tag is derived deterministically from the Temporal run id (SPEC §4.2), so it is
// reconstructable from the run alone and unique per run (a leaked hold from a
// dead run never collides with a future run's own hold).

const (
	// holdTagPrefix namespaces the run's hold tag so an operator can recognise a
	// tape-archiver hold among any others on a snapshot. The full tag is this
	// prefix plus the Temporal run id (see HoldTag).
	holdTagPrefix = "tape-archiver-hold-"

	// holdTimeout bounds the hold and release activities. Each is a handful of
	// cheap `zfs hold`/`zfs release` CLI calls per source snapshot, so minutes
	// are ample even for a run naming many sources.
	holdTimeout = 5 * time.Minute
)

// HoldTag returns the run-scoped ZFS hold tag for the given Temporal run id. It
// is deterministic and unique per run: reconstructable from the run alone (no
// catalog, SPEC §4.2), and distinct across runs so one run's hold never blocks
// or collides with another's.
func HoldTag(runID string) string {
	return holdTagPrefix + runID
}

// snapshotHolder is the seam the zfs hold/release functions are wrapped behind,
// injected so the hold activities are unit-testable without a real pool (mirrors
// the poolInspector seam in resolve.go).
type snapshotHolder interface {
	Hold(ctx context.Context, tag, snapshot string) error
	Release(ctx context.Context, tag, snapshot string) error
}

// zfsHolder is the production snapshotHolder: a thin adapter over the
// package-level zfs functions, which shell out to the zfs CLI on the data worker.
type zfsHolder struct{}

func (zfsHolder) Hold(ctx context.Context, tag, snapshot string) error {
	return zfs.Hold(ctx, tag, snapshot)
}

func (zfsHolder) Release(ctx context.Context, tag, snapshot string) error {
	return zfs.Release(ctx, tag, snapshot)
}

// HoldActivities hosts the source-snapshot hold and release activities, which run
// where the ZFS pool is reachable (the data worker, SPEC §4.1).
type HoldActivities struct {
	holder snapshotHolder
}

// newHoldActivities returns the production hold activities, holding snapshots
// through the zfs CLI on the data worker.
func newHoldActivities() *HoldActivities {
	return &HoldActivities{holder: zfsHolder{}}
}

// HoldInput is the payload for both hold activities: the run-scoped tag and the
// ZFS snapshot paths to hold or release.
type HoldInput struct {
	// Tag is the run-scoped hold tag (HoldTag).
	Tag string
	// Snapshots are the absolute ZFS snapshot paths to hold or release, e.g.
	// bulk-pool-01/archive@daily-2026-06-28.
	Snapshots []string
}

// HoldSnapshots places the run's hold on every snapshot in the input. It is
// idempotent per snapshot (zfs.Hold treats an already-present tag as success), so
// an activity retry that re-holds an already-held snapshot is a no-op.
func (a *HoldActivities) HoldSnapshots(ctx context.Context, input HoldInput) error {
	for _, snapshot := range input.Snapshots {
		if err := a.holder.Hold(ctx, input.Tag, snapshot); err != nil {
			return fmt.Errorf("hold snapshot %s with tag %s: %w", snapshot, input.Tag, err)
		}
	}

	slog.InfoContext(ctx, "hold: pinned source snapshots against pruning for the run",
		"snapshots", len(input.Snapshots), "tag", input.Tag)

	return nil
}

// ReleaseSnapshots removes the run's hold from every snapshot in the input. It is
// tolerant of an already-absent hold (zfs.Release treats "no such tag" as
// success), so releasing on an exit path where a snapshot was never held — or a
// retry that re-releases — is a no-op.
func (a *HoldActivities) ReleaseSnapshots(ctx context.Context, input HoldInput) error {
	for _, snapshot := range input.Snapshots {
		if err := a.holder.Release(ctx, input.Tag, snapshot); err != nil {
			return fmt.Errorf("release snapshot %s tag %s: %w", snapshot, input.Tag, err)
		}
	}

	slog.InfoContext(ctx, "hold: released the run's holds on source snapshots",
		"snapshots", len(input.Snapshots), "tag", input.Tag)

	return nil
}

// holdSnapshotPaths flattens the resolved work list to the distinct ZFS snapshot
// paths the run should hold: every ResolvedArchive's snapshots by ZFSPath,
// skipping any raw-dataset source without an "@" (a bare dataset has no snapshot
// to hold — skipped, not errored) and de-duplicating so a snapshot shared across
// archives is held once.
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

// holdSnapshots pins every resolved source snapshot with the run's hold, run on
// the data queue where the pool is reachable. It returns an error on failure: a
// hold failure fails the run before any staging begins, since the whole point of
// the hold is to guarantee the snapshot is pinned before hours of Prepare work.
// A run with no snapshots to hold (only bare-dataset sources, or no sources) is a
// no-op that dispatches nothing.
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
	})

	var activities *HoldActivities

	return workflow.ExecuteActivity(actx, activities.HoldSnapshots, input).Get(actx, nil)
}

// releaseSnapshots removes the run's hold from every resolved source snapshot. It
// re-derives the identical tag and snapshot list as holdSnapshots and runs on a
// disconnected context so the release still fires when the workflow is cancelled
// (modeled on notifyFailure). It logs but never returns a release error: a failed
// release must not mask the run's own outcome — a leaked hold is observable and
// manually clearable (docs/maintenance.md). It is deferred once, *before* the hold
// is placed, so it fires at workflow return on every exit path — including when
// HoldSnapshots fails after pinning only a subset of the snapshots. Releasing a
// snapshot that was never held is a no-op, so arming it ahead of the hold is safe.
func releaseSnapshots(ctx workflow.Context, state *runState) {
	snapshots := holdSnapshotPaths(state)
	if len(snapshots) == 0 {
		return
	}

	input := HoldInput{
		Tag:       HoldTag(workflow.GetInfo(ctx).WorkflowExecution.RunID),
		Snapshots: snapshots,
	}

	// A disconnected context is not cancelled when the workflow is, so the holds
	// are released even on cancellation (SPEC §4.3).
	disconnected, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()

	actx := workflow.WithActivityOptions(disconnected, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: holdTimeout,
	})

	var activities *HoldActivities
	if err := workflow.ExecuteActivity(actx, activities.ReleaseSnapshots, input).Get(actx, nil); err != nil {
		workflow.GetLogger(ctx).Error("failed to release source snapshot holds",
			"tag", input.Tag,
			"error", err,
		)
	}
}
