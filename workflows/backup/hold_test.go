package backup

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// recordingHolder is a snapshotHolder that records every hold and release it is
// asked to perform, keyed by tag, so a test can assert which snapshots a run
// pinned and released. holdErr, when set, makes every Hold fail.
type recordingHolder struct {
	mu       sync.Mutex
	held     map[string][]string
	released map[string][]string
	holdErr  error
}

func newRecordingHolder() *recordingHolder {
	return &recordingHolder{
		held:     map[string][]string{},
		released: map[string][]string{},
	}
}

func (r *recordingHolder) Hold(_ context.Context, tag, snapshot string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.holdErr != nil {
		return r.holdErr
	}

	r.held[tag] = append(r.held[tag], snapshot)

	return nil
}

func (r *recordingHolder) Release(_ context.Context, tag, snapshot string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.released[tag] = append(r.released[tag], snapshot)

	return nil
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

// holdTestParam is the serializable input to holdTestWorkflow.
type holdTestParam struct {
	Archives []ResolvedArchive
	// Mode selects the exit path after the hold: "success" returns cleanly,
	// "fail" returns an error, "cancel" blocks until the test cancels the run.
	Mode string
}

// holdTestWorkflow exercises the workflow hold/release helpers exactly as Backup
// does: it holds every resolved source snapshot, then defers the release (only
// after a successful hold), then exits via the requested path. It returns the
// run's hold tag so a test can key the recorder by it.
func holdTestWorkflow(ctx workflow.Context, param holdTestParam) (string, error) {
	state := &runState{resolved: param.Archives}
	tag := HoldTag(workflow.GetInfo(ctx).WorkflowExecution.RunID)

	if err := holdSnapshots(ctx, state); err != nil {
		return tag, err
	}

	defer releaseSnapshots(ctx, state)

	switch param.Mode {
	case "fail":
		return tag, errors.New("boom")
	case "cancel":
		// Block until the test cancels the run; on cancellation the deferred
		// release still fires because it runs on a disconnected context.
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

// TestHoldSnapshotPaths covers the pure snapshot-selection logic: it flattens the
// resolved work list, skips bare-dataset sources (no "@", nothing to hold), and
// de-duplicates a snapshot shared across archives, preserving first-seen order.
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

// TestHoldSnapshotsTagsAndSkipsBareDatasets asserts the run holds every resolved
// source snapshot under a run-id-derived tag, skipping bare datasets and holding
// a shared snapshot once, and releases the same set on the success exit path.
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

// TestHoldReleaseOnFailure asserts the run releases its holds when a later phase
// fails (AC2, failure exit path).
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

// TestHoldReleaseOnCancellation asserts the run releases its holds even when the
// workflow is cancelled — proving the release runs on a disconnected context
// (AC2, cancellation exit path).
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

// TestHoldSnapshotsFailsRunOnHoldError asserts a hold failure surfaces from the
// hold helper (the workflow treats it as fatal, failing the run before Prepare)
// and that no release is attempted when the hold never succeeded.
func TestHoldSnapshotsFailsRunOnHoldError(t *testing.T) {
	t.Parallel()

	holder := newRecordingHolder()
	holder.holdErr = errors.New("pool offline")
	env := newHoldEnv(holder)

	archives := []ResolvedArchive{{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}}}

	env.ExecuteWorkflow(holdTestWorkflow, holdTestParam{Archives: archives, Mode: "success"})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError())
	assert.Empty(t, holder.releasedTags(), "no release when the hold never succeeded")
}

// TestHoldActivitiesIterate covers the hold/release activities directly: each
// visits every snapshot, HoldSnapshots surfaces the first hold error, and
// ReleaseSnapshots releases all snapshots under the run tag.
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
