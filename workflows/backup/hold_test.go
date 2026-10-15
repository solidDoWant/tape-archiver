package backup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// recordingHolder records holds and releases by tag. holdErr fails Hold (from
// the holdErrAfter'th snapshot when set, for a partial hold); holdBlocks blocks
// Hold until the run is cancelled.
type recordingHolder struct {
	mu           sync.Mutex
	held         map[string][]string
	released     map[string][]string
	holdErr      error
	holdErrAfter int
	holdBlocks   bool
	holds        int
	// releaseErr fails every Release of that snapshot (only the first
	// releaseFailures attempts when set); releases counts attempts.
	releaseErr      map[string]error
	releases        map[string]int
	releaseFailures int
}

func newRecordingHolder() *recordingHolder {
	return &recordingHolder{
		held:     map[string][]string{},
		released: map[string][]string{},
	}
}

func (r *recordingHolder) Hold(ctx context.Context, tag, snapshot string) error {
	if r.holdBlocks {
		<-ctx.Done()

		return ctx.Err()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.holds++

	// holdErrAfter: hold the first holdErrAfter-1 snapshots, then fail. Zero (with
	// holdErr) fails every hold.
	if r.holdErr != nil && r.holds >= r.holdErrAfter {
		return r.holdErr
	}

	r.held[tag] = append(r.held[tag], snapshot)

	return nil
}

func (r *recordingHolder) Release(_ context.Context, tag, snapshot string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.releases == nil {
		r.releases = map[string]int{}
	}

	r.releases[snapshot]++

	if err := r.releaseErr[snapshot]; err != nil && (r.releaseFailures == 0 || r.releases[snapshot] <= r.releaseFailures) {
		return err
	}

	r.released[tag] = append(r.released[tag], snapshot)

	return nil
}

func (r *recordingHolder) releaseAttempts(snapshot string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.releases[snapshot]
}

func (r *recordingHolder) heldFor(tag string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.held[tag]...)
}

func (r *recordingHolder) releasedFor(tag string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.released[tag]...)
}

func (r *recordingHolder) releasedTags() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	tags := make([]string, 0, len(r.released))
	for tag := range r.released {
		tags = append(tags, tag)
	}

	return tags
}

type holdTestParam struct {
	Archives []ResolvedArchive
	// Mode is "success", "fail", or "cancel" (blocks until the test cancels).
	Mode string
}

// holdTestWorkflow mirrors Backup: arm the release, hold, then exit via Mode.
// It returns the hold tag.
func holdTestWorkflow(ctx workflow.Context, param holdTestParam) (string, error) {
	state := &runState{resolved: param.Archives}
	tag := HoldTag(workflow.GetInfo(ctx).WorkflowExecution.RunID)

	defer func() { _ = releaseSnapshots(ctx, state) }()

	if err := holdSnapshots(ctx, state); err != nil {
		return tag, err
	}

	switch param.Mode {
	case "fail":
		return tag, errors.New("boom")
	case "cancel":
		// The deferred release still fires: it runs on a disconnected context.
		_ = workflow.Sleep(ctx, time.Hour)
	}

	return tag, nil
}

func newHoldEnv(holder snapshotHolder) *testsuite.TestWorkflowEnvironment {
	var suite testsuite.WorkflowTestSuite

	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(holdTestWorkflow)
	env.RegisterActivity(&HoldActivities{holder: holder})

	return env
}

// TestHoldSnapshotPaths asserts bare datasets are skipped and shared snapshots
// de-duplicated in first-seen order.
func TestHoldSnapshotPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state *runState
		want  []string
	}{
		{
			name:  "no sources",
			state: &runState{},
			want:  nil,
		},
		{
			name: "skips bare-dataset source with no snapshot",
			state: &runState{resolved: []ResolvedArchive{
				{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/rawdataset"}}},
			}},
			want: nil,
		},
		{
			name: "holds snapshots and skips bare datasets and dedupes",
			state: &runState{resolved: []ResolvedArchive{
				{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}, {ZFSPath: "pool/b@snap2"}}},
				{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/rawdataset"}}},
				{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}},
			}},
			want: []string{"pool/a@snap1", "pool/b@snap2"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := holdSnapshotPaths(test.state)
			assert.Equal(t, test.want, got)
		})
	}
}

// TestHoldSnapshotsTagsAndSkipsBareDatasets asserts holds use the run-id tag and
// the same set is released on success.
func TestHoldSnapshotsTagsAndSkipsBareDatasets(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	env := newHoldEnv(holder)

	archives := []ResolvedArchive{
		{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}, {ZFSPath: "pool/b@snap2"}}},
		{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/rawdataset"}}},
		{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}},
	}

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "success"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var tag string
	require.NoError(t, env.GetWorkflowResult(&tag))

	assert.Contains(t, tag, holdTagPrefix, "hold tag is run-id-derived")
	assert.ElementsMatch(t, []string{"pool/a@snap1", "pool/b@snap2"}, holder.heldFor(tag),
		"bare datasets skipped, shared snapshot held once")
	assert.ElementsMatch(t, []string{"pool/a@snap1", "pool/b@snap2"}, holder.releasedFor(tag),
		"holds released on the success exit path")
}

// TestHoldReleaseOnFailure: holds are released when a later phase fails.
func TestHoldReleaseOnFailure(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	env := newHoldEnv(holder)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}}}

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "fail"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())

	tags := holder.releasedTags()
	require.Len(t, tags, 1, "the run released its hold on failure")
	assert.Contains(t, tags[0], holdTagPrefix)
	assert.ElementsMatch(t, []string{"pool/a@snap1"}, holder.releasedFor(tags[0]))
}

// TestHoldReleaseOnCancellation: holds are released on cancellation, proving the
// release runs on a disconnected context.
func TestHoldReleaseOnCancellation(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	env := newHoldEnv(holder)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}}}

	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Millisecond)

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "cancel"})

	require.True(t, env.IsWorkflowCompleted())

	tags := holder.releasedTags()
	require.Len(t, tags, 1, "the run released its hold on cancellation")
	assert.ElementsMatch(t, []string{"pool/a@snap1"}, holder.releasedFor(tags[0]))
}

// TestHoldSnapshotsFailsRunOnHoldError asserts a hold failure surfaces and the
// release still fires, since it is armed before the hold (issue #218).
func TestHoldSnapshotsFailsRunOnHoldError(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	holder.holdErr = errors.New("pool offline")
	env := newHoldEnv(holder)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}}}

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "success"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())

	tags := holder.releasedTags()
	require.Len(t, tags, 1, "the run releases even when the hold never succeeded")
	assert.Contains(t, tags[0], holdTagPrefix)
	assert.ElementsMatch(t, []string{"pool/a@snap1"}, holder.releasedFor(tags[0]),
		"no resolved snapshot retains the run's hold tag")
}

// TestHoldReleaseOnPartialHoldFailure: a hold failing after pinning a subset
// still releases every snapshot, so none leaks past the dead run.
func TestHoldReleaseOnPartialHoldFailure(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	holder.holdErr = errors.New("snapshot pruned mid-run")
	holder.holdErrAfter = 3 // pin snap1 and snap2, then fail on snap3

	env := newHoldEnv(holder)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{
		{ZFSPath: "pool/a@snap1"},
		{ZFSPath: "pool/b@snap2"},
		{ZFSPath: "pool/c@snap3"},
	}}}

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "success"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())

	tags := holder.releasedTags()
	require.Len(t, tags, 1, "the run released its holds despite the partial failure")
	tag := tags[0]
	assert.Contains(t, tag, holdTagPrefix)

	assert.ElementsMatch(t, []string{"pool/a@snap1", "pool/b@snap2"}, holder.heldFor(tag),
		"the run pinned a subset before the hold failed")

	assert.ElementsMatch(t, []string{"pool/a@snap1", "pool/b@snap2", "pool/c@snap3"},
		holder.releasedFor(tag), "no resolved snapshot retains the run's hold tag")
}

// TestHoldReleaseOnCancelDuringHold: cancelling while the hold never completes
// still releases (unlike TestHoldReleaseOnCancellation, which cancels after a
// successful hold).
func TestHoldReleaseOnCancelDuringHold(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	holder.holdBlocks = true
	env := newHoldEnv(holder)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}}}

	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Millisecond)

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "success"})

	require.True(t, env.IsWorkflowCompleted())

	tags := holder.releasedTags()
	require.Len(t, tags, 1, "the run released its hold after being cancelled mid-hold")
	assert.ElementsMatch(t, []string{"pool/a@snap1"}, holder.releasedFor(tags[0]),
		"no resolved snapshot retains the run's hold tag")
}

// TestHoldActivitiesIterate covers the activities directly: every snapshot is
// visited and the first hold error surfaces.
func TestHoldActivitiesIterate(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	activities := &HoldActivities{holder: holder}
	input := HoldInput{Tag: "tag-1", Snapshots: []string{"pool/a@snap1", "pool/b@snap2"}}

	require.NoError(t, activities.HoldSnapshots(t.Context(), input))
	assert.Equal(t, []string{"pool/a@snap1", "pool/b@snap2"}, holder.heldFor("tag-1"))

	require.NoError(t, activities.ReleaseSnapshots(t.Context(), input))
	assert.Equal(t, []string{"pool/a@snap1", "pool/b@snap2"}, holder.releasedFor("tag-1"))

	failing := &HoldActivities{holder: &recordingHolder{
		held:     map[string][]string{},
		released: map[string][]string{},
		holdErr:  errors.New("boom"),
	}}
	require.Error(t, failing.HoldSnapshots(t.Context(), input))
}

// TestHoldVanishedSnapshotFailsPromptly (#387): a snapshot pruned before the hold
// fails the hold without retrying, and every snapshot is still released.
func TestHoldVanishedSnapshotFailsPromptly(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	holder.holdErr = fmt.Errorf("zfs hold: %w", zfs.ErrNotExist)
	holder.holdErrAfter = 3

	env := newHoldEnv(holder)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{
		{ZFSPath: "pool/a@snap1"},
		{ZFSPath: "pool/b@snap2"},
		{ZFSPath: "pool/c@snap3"},
	}}}

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "success"})

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), "does not exist")
	assert.Equal(t, 3, holder.holds, "a vanished snapshot must not be retried")

	tags := holder.releasedTags()
	require.Len(t, tags, 1)
	assert.ElementsMatch(t, []string{"pool/a@snap1", "pool/b@snap2", "pool/c@snap3"}, holder.releasedFor(tags[0]))
}

// TestHoldReleaseFailureIsBounded (#387): a release that keeps failing is retried
// a bounded number of times, does not stop the other snapshots' release, and the
// workflow still closes.
func TestHoldReleaseFailureIsBounded(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	holder.releaseErr = map[string]error{"pool/b@snap2": errors.New("pool busy")}

	env := newHoldEnv(holder)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{
		{ZFSPath: "pool/a@snap1"},
		{ZFSPath: "pool/b@snap2"},
		{ZFSPath: "pool/c@snap3"},
	}}}

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "fail"})

	require.True(t, env.IsWorkflowCompleted())
	require.ErrorContains(t, env.GetWorkflowError(), "boom", "the run's own error survives the failed release")
	assert.Equal(t, releaseMaxAttempts, holder.releaseAttempts("pool/b@snap2"))

	tags := holder.releasedTags()
	require.Len(t, tags, 1)
	assert.Contains(t, holder.releasedFor(tags[0]), "pool/a@snap1")
	assert.Contains(t, holder.releasedFor(tags[0]), "pool/c@snap3")
}

// TestHoldReleaseRidesOutTransients (#387): a release that fails transiently for
// longer than the hold's bound is retried until it succeeds, so a brief zfs
// fault does not leak the hold.
func TestHoldReleaseRidesOutTransients(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	holder.releaseErr = map[string]error{"pool/b@snap2": errors.New("pool busy")}
	holder.releaseFailures = releaseMaxAttempts - 1

	env := newHoldEnv(holder)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{
		{ZFSPath: "pool/a@snap1"},
		{ZFSPath: "pool/b@snap2"},
	}}}

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "success"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	assert.Equal(t, releaseMaxAttempts, holder.releaseAttempts("pool/b@snap2"))

	tags := holder.releasedTags()
	require.Len(t, tags, 1)
	assert.Contains(t, holder.releasedFor(tags[0]), "pool/b@snap2")
}

// TestHoldReleaseRetryWindow (#387): a release that never succeeds keeps
// retrying for minutes, not seconds, yet gives up within its bounded window so
// it cannot block the failure alert forever.
func TestHoldReleaseRetryWindow(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	holder.releaseErr = map[string]error{"pool/a@snap1": errors.New("pool busy")}

	env := newHoldEnv(holder)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}}}

	start := env.Now()

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "success"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	elapsed := env.Now().Sub(start)
	assert.GreaterOrEqual(t, elapsed, 3*time.Minute)
	assert.LessOrEqual(t, elapsed, releaseWindow)
}
