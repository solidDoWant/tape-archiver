package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/zfs"
)

// testRunID is the testsuite's fixed run id.
const testRunID = "default-test-run-id"

func TestRunSnapshotName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		runID string
		want  string
	}{
		{name: "uuid kept", runID: "0199f3c1-7d2a-7b4e-9a51-3c1f0e2d4b6a", want: "tape-archiver-0199f3c1-7d2a-7b4e-9a51-3c1f0e2d4b6a"},
		{name: "zfs-legal punctuation kept", runID: "a_b.c:d", want: "tape-archiver-a_b.c:d"},
		{name: "invalid characters replaced", runID: "a/b@c d%e", want: "tape-archiver-a-b-c-d-e"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, RunSnapshotName(test.runID))
		})
	}
}

// TestPlanRunScopedSnapshots: only bare datasets become run-scoped snapshots;
// explicit and k8s snapshots are untouched, and a shared dataset is taken once.
func TestPlanRunScopedSnapshots(t *testing.T) {
	t.Parallel()

	k8s := ResolvedSnapshot{ZFSPath: "pool/pvc@snapshot-1", Dataset: "pool/pvc", SnapshotName: "snapshot-1", PVC: "data"}
	resolved := []ResolvedArchive{
		{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/live"}}},
		{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/archive@daily"}, k8s}},
		{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/live"}, {ZFSPath: "other/media"}}},
	}

	paths := planRunScopedSnapshots(resolved, "run-1")

	want := ResolvedSnapshot{
		ZFSPath: "pool/live@tape-archiver-run-1", Dataset: "pool/live", SnapshotName: "tape-archiver-run-1", RunScoped: true,
	}
	assert.Equal(t, []string{"pool/live@tape-archiver-run-1", "other/media@tape-archiver-run-1"}, paths)
	assert.Equal(t, want, resolved[0].Snapshots[0])
	assert.Equal(t, []ResolvedSnapshot{{ZFSPath: "pool/archive@daily"}, k8s}, resolved[1].Snapshots)
	assert.Equal(t, want, resolved[2].Snapshots[0])
	assert.Equal(t, paths, runScopedSnapshotPaths(&runState{resolved: resolved}))
	assert.Equal(t, []string{"pool/live@tape-archiver-run-1", "pool/archive@daily", "pool/pvc@snapshot-1",
		"other/media@tape-archiver-run-1"}, holdSnapshotPaths(&runState{resolved: resolved}),
		"run-scoped snapshots are held like any other source snapshot")
}

// fakeSnapshot is one snapshot in fakeZFS.
type fakeSnapshot struct {
	runID       string // RunIDProperty value; "" when unset
	inheritedID bool   // the property is inherited, not local
	holds       []string
	creation    int64
}

// fakeZFS is an in-memory snapshotManager. extraRecords are appended to
// LocalProperty output, as a value with an embedded newline would fabricate.
type fakeZFS struct {
	mu           sync.Mutex
	snapshots    map[string]*fakeSnapshot
	extraRecords []zfs.PropertyRecord
	createCalls  [][]string
	destroyed    []string
	released     []string
	destroyErr   error
	createErr    error
	nextCreation int64
}

func newFakeZFS(snapshots map[string]*fakeSnapshot) *fakeZFS {
	if snapshots == nil {
		snapshots = make(map[string]*fakeSnapshot)
	}

	return &fakeZFS{snapshots: snapshots, nextCreation: 1_760_000_000}
}

func notExist(name string) error {
	return fmt.Errorf("cannot open '%s': %w", name, zfs.ErrNotExist)
}

func (f *fakeZFS) CreateSnapshots(_ context.Context, properties map[string]string, snapshots []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createErr != nil {
		return f.createErr
	}

	f.createCalls = append(f.createCalls, append([]string(nil), snapshots...))

	for _, snapshot := range snapshots {
		if _, ok := f.snapshots[snapshot]; ok {
			return fmt.Errorf("cannot create snapshot '%s': dataset already exists", snapshot)
		}

		f.nextCreation++
		f.snapshots[snapshot] = &fakeSnapshot{runID: properties[RunIDProperty], creation: f.nextCreation}
	}

	return nil
}

func (f *fakeZFS) DestroySnapshot(_ context.Context, snapshot string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.destroyErr != nil {
		return f.destroyErr
	}

	existing, ok := f.snapshots[snapshot]
	if !ok {
		return nil
	}

	if len(existing.holds) > 0 {
		return fmt.Errorf("cannot destroy snapshot %s: dataset is busy", snapshot)
	}

	delete(f.snapshots, snapshot)
	f.destroyed = append(f.destroyed, snapshot)

	return nil
}

func (f *fakeZFS) Creation(_ context.Context, name string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if existing, ok := f.snapshots[name]; ok {
		return existing.creation, nil
	}

	return 0, notExist(name)
}

func (f *fakeZFS) UserProperty(_ context.Context, name, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing, ok := f.snapshots[name]
	switch {
	case !ok:
		return "", notExist(name)
	case existing.runID == "":
		return "-", nil
	default:
		return existing.runID, nil
	}
}

func (f *fakeZFS) PropertySource(_ context.Context, name, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing, ok := f.snapshots[name]
	switch {
	case !ok:
		return "", notExist(name)
	case existing.runID == "":
		return "-", nil
	case existing.inheritedID:
		return "inherited from pool", nil
	default:
		return "local", nil
	}
}

func (f *fakeZFS) LocalProperty(context.Context, string, string, string) ([]zfs.PropertyRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var records []zfs.PropertyRecord

	for name, snapshot := range f.snapshots {
		if snapshot.runID != "" && !snapshot.inheritedID {
			records = append(records, zfs.PropertyRecord{Name: name, Value: snapshot.runID})
		}
	}

	slices.SortFunc(records, func(a, b zfs.PropertyRecord) int { return strings.Compare(a.Name, b.Name) })

	return append(records, f.extraRecords...), nil
}

func (f *fakeZFS) Holds(_ context.Context, snapshot string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if existing, ok := f.snapshots[snapshot]; ok {
		return append([]string(nil), existing.holds...), nil
	}

	return nil, notExist(snapshot)
}

func (f *fakeZFS) Release(_ context.Context, tag, snapshot string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if existing, ok := f.snapshots[snapshot]; ok {
		existing.holds = slices.DeleteFunc(existing.holds, func(held string) bool { return held == tag })
		f.released = append(f.released, tag+" "+snapshot)
	}

	return nil
}

func (f *fakeZFS) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	names := make([]string, 0, len(f.snapshots))
	for name := range f.snapshots {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// TestCreateRunSnapshots covers taking the run's snapshots: one call per pool
// with the run-id property, a retry keeping its own snapshots, and a foreign
// snapshot under the run-scoped name refused.
func TestCreateRunSnapshots(t *testing.T) {
	t.Parallel()

	ours := "pool/b@" + RunSnapshotName("run-1")

	tests := []struct {
		name      string
		existing  map[string]*fakeSnapshot
		snapshots []string
		wantCalls [][]string
		assertErr require.ErrorAssertionFunc
	}{
		{
			name:      "one atomic call per pool",
			snapshots: []string{"pool/a@" + RunSnapshotName("run-1"), "other/x@" + RunSnapshotName("run-1"), ours},
			wantCalls: [][]string{{"other/x@" + RunSnapshotName("run-1")}, {"pool/a@" + RunSnapshotName("run-1"), ours}},
		},
		{
			name:      "retry keeps snapshots this run already took",
			existing:  map[string]*fakeSnapshot{ours: {runID: "run-1", creation: 42}},
			snapshots: []string{"pool/a@" + RunSnapshotName("run-1"), ours},
			wantCalls: [][]string{{"pool/a@" + RunSnapshotName("run-1")}},
		},
		{
			name:      "name taken by a snapshot without this run's id",
			existing:  map[string]*fakeSnapshot{ours: {}},
			snapshots: []string{ours},
			assertErr: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			fake := newFakeZFS(test.existing)
			activities := &RunSnapshotActivities{zfs: fake}

			created, err := activities.CreateRunSnapshots(t.Context(), RunSnapshotInput{RunID: "run-1", Snapshots: test.snapshots})
			test.assertErr(t, err)
			assert.Equal(t, test.wantCalls, fake.createCalls)

			if err != nil {
				assert.Empty(t, fake.destroyed, "a refused name is never destroyed")

				return
			}

			require.Len(t, created, len(test.snapshots))

			for index, snapshot := range test.snapshots {
				assert.Equal(t, snapshot, created[index].ZFSPath)
				assert.Equal(t, fake.snapshots[snapshot].creation, created[index].Creation)
				assert.Equal(t, "run-1", fake.snapshots[snapshot].runID, "created with the run-id property")
			}
		})
	}
}

// TestCreateRunSnapshotsRemovesLeaks: before taking its own snapshots a run
// destroys every other run's run-scoped snapshot (releasing tape-archiver holds
// first) and nothing that lacks proof of tape-archiver ownership.
func TestCreateRunSnapshotsRemovesLeaks(t *testing.T) {
	t.Parallel()

	leaked := "pool/a@" + RunSnapshotName("old-run")
	heldLeak := "pool/b@" + RunSnapshotName("older-run")
	foreignHold := "pool/c@" + RunSnapshotName("other-run")
	current := "pool/d@" + RunSnapshotName("run-1")

	fake := newFakeZFS(map[string]*fakeSnapshot{
		leaked:      {runID: "old-run"},
		heldLeak:    {runID: "older-run", holds: []string{HoldTag("older-run")}},
		foreignHold: {runID: "other-run", holds: []string{"sanoid"}},
		current:     {runID: "run-1"},
		// Name prefix alone is not proof of ownership.
		"pool/e@" + RunSnapshotName("no-property"): {},
		// Property present but the name is not that run's snapshot name.
		"pool/f@daily": {runID: "old-run"},
		// Property inherited, not set at creation.
		"pool/g@" + RunSnapshotName("old-run"): {runID: "old-run", inheritedID: true},
	})
	// Fabricated records naming snapshots without a local run-id property.
	fake.extraRecords = []zfs.PropertyRecord{
		{Name: "pool/e@" + RunSnapshotName("no-property"), Value: "no-property"},
		{Name: "pool/g@" + RunSnapshotName("old-run"), Value: "old-run"},
	}

	activities := &RunSnapshotActivities{zfs: fake}

	_, err := activities.CreateRunSnapshots(t.Context(), RunSnapshotInput{RunID: "run-1"})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{leaked, heldLeak}, fake.destroyed)
	assert.Equal(t, []string{HoldTag("older-run") + " " + heldLeak}, fake.released,
		"only tape-archiver's own hold is released")
	assert.Equal(t, []string{
		foreignHold, current,
		"pool/e@" + RunSnapshotName("no-property"),
		"pool/f@daily",
		"pool/g@" + RunSnapshotName("old-run"),
	}, fake.names())
}

// TestDestroyRunSnapshots: destroy is idempotent and never touches a snapshot
// without this run's id.
func TestDestroyRunSnapshots(t *testing.T) {
	t.Parallel()

	ours := "pool/a@" + RunSnapshotName("run-1")
	foreign := "pool/b@" + RunSnapshotName("run-1")

	tests := []struct {
		name       string
		existing   map[string]*fakeSnapshot
		destroyErr error
		wantLeft   []string
		assertErr  require.ErrorAssertionFunc
	}{
		{
			name:     "destroys this run's snapshots only",
			existing: map[string]*fakeSnapshot{ours: {runID: "run-1"}, foreign: {runID: "someone-else"}},
			wantLeft: []string{foreign},
		},
		{
			name:     "already gone is success",
			wantLeft: []string{},
		},
		{
			name:       "destroy failure is returned",
			existing:   map[string]*fakeSnapshot{ours: {runID: "run-1"}},
			destroyErr: errors.New("dataset is busy"),
			wantLeft:   []string{ours},
			assertErr:  require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			fake := newFakeZFS(test.existing)
			fake.destroyErr = test.destroyErr

			err := (&RunSnapshotActivities{zfs: fake}).DestroyRunSnapshots(t.Context(),
				RunSnapshotInput{RunID: "run-1", Snapshots: []string{ours, foreign}})
			test.assertErr(t, err)
			assert.Equal(t, test.wantLeft, fake.names())
		})
	}
}

// TestBackupRunScopedSnapshotLifecycle: a live dataset is snapshotted after
// Resolve and before the hold, Prepare stages from that snapshot, and on every
// exit path the hold is released and then the snapshot destroyed.
func TestBackupRunScopedSnapshotLifecycle(t *testing.T) {
	t.Parallel()

	snapshot := "pool/live@" + RunSnapshotName(testRunID)

	tests := []struct {
		name       string
		mode       string
		createErr  error
		destroyErr error
		wantCalls  []string
		wantErr    string
	}{
		{name: "success", mode: "success", wantCalls: []string{"create", "hold", "prepare", "release", "destroy"}},
		{name: "prepare fails", mode: "fail", wantCalls: []string{"create", "hold", "prepare", "release", "destroy"}, wantErr: "phase Prepare"},
		{name: "cancelled", mode: "cancel", wantCalls: []string{"create", "hold", "prepare", "release", "destroy"}, wantErr: "canceled"},
		{
			name: "snapshot fails", mode: "success", createErr: errors.New("out of space"),
			wantCalls: []string{"create", "release", "destroy"}, wantErr: "phase Snapshot",
		},
		{
			name: "failed run names a leaked snapshot", mode: "fail", destroyErr: errors.New("dataset is busy"),
			wantCalls: []string{"create", "hold", "prepare", "release", "destroy"}, wantErr: "destroy run-scoped snapshots",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			env := newBackupEnv(t)
			expectReportDeliverSuccess(env)

			var (
				mu           sync.Mutex
				calls        []string
				createInput  RunSnapshotInput
				holdInput    HoldInput
				prepareInput PrepareInput
				destroyInput RunSnapshotInput
			)

			record := func(call string) {
				mu.Lock()
				defer mu.Unlock()

				if len(calls) == 0 || calls[len(calls)-1] != call {
					calls = append(calls, call)
				}
			}

			env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
				Return([]ResolvedArchive(nil), nil)
			env.OnActivity((&ResolveDataActivities{}).ResolveAndCheck, mock.Anything, mock.Anything).
				Return([]ResolvedArchive{{Label: "live", Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/live"}}}}, nil)
			env.OnActivity((&RunSnapshotActivities{}).CreateRunSnapshots, mock.Anything, mock.Anything).
				Return(func(_ context.Context, input RunSnapshotInput) ([]CreatedSnapshot, error) {
					record("create")

					createInput = input

					if test.createErr != nil {
						return nil, test.createErr
					}

					return []CreatedSnapshot{{ZFSPath: snapshot, Creation: 1_760_000_000}}, nil
				})
			env.OnActivity((&HoldActivities{}).HoldSnapshots, mock.Anything, mock.Anything).
				Return(func(_ context.Context, input HoldInput) error {
					record("hold")

					holdInput = input

					return nil
				})

			env.OnActivity((&PrepareActivities{}).PrepareArchives, mock.Anything, mock.Anything).
				Return(func(_ context.Context, input PrepareInput) ([]StagedArchive, error) {
					record("prepare")

					prepareInput = input

					if test.mode == "fail" {
						return nil, errors.New("boom")
					}

					return nil, nil
				})
			if test.mode == "cancel" {
				// Cancelled while Pack is in flight, after staging from the snapshot.
				env.OnActivity((&PackActivities{}).Pack, mock.Anything, mock.Anything).
					After(time.Hour).Return(TapePlan{}, nil)
				env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)
			}

			env.OnActivity((&HoldActivities{}).ReleaseSnapshots, mock.Anything, mock.Anything).
				Return(func(context.Context, HoldInput) error {
					record("release")

					return nil
				})
			env.OnActivity((&RunSnapshotActivities{}).DestroyRunSnapshots, mock.Anything, mock.Anything).
				Return(func(_ context.Context, input RunSnapshotInput) error {
					record("destroy")

					destroyInput = input

					return test.destroyErr
				})

			cfg := validBackupConfig()
			cfg.Sources[0].ZFSPath.Name = "pool/live"

			env.ExecuteWorkflow(Backup, cfg)
			require.True(t, env.IsWorkflowCompleted())

			if test.wantErr == "" {
				require.NoError(t, env.GetWorkflowError())
			} else {
				require.ErrorContains(t, env.GetWorkflowError(), test.wantErr)
			}

			assert.Equal(t, test.wantCalls, calls)
			assert.Equal(t, RunSnapshotInput{RunID: testRunID, Snapshots: []string{snapshot}}, createInput)
			assert.Equal(t, RunSnapshotInput{RunID: testRunID, Snapshots: []string{snapshot}}, destroyInput)

			if test.createErr != nil {
				return
			}

			assert.Equal(t, []string{snapshot}, holdInput.Snapshots)
			require.Len(t, prepareInput.Archives, 1)
			assert.Equal(t, []ResolvedSnapshot{{
				ZFSPath: snapshot, Dataset: "pool/live", SnapshotName: RunSnapshotName(testRunID),
				RunScoped: true, CreatedAt: time.Unix(1_760_000_000, 0).UTC(),
			}}, prepareInput.Archives[0].Snapshots, "Prepare stages from the run's snapshot")
		})
	}
}

// TestReportNamesRunScopedSnapshot: the report names a run-scoped snapshot and
// when it was taken.
func TestReportNamesRunScopedSnapshot(t *testing.T) {
	t.Parallel()

	resolved := ResolvedArchive{Snapshots: []ResolvedSnapshot{
		{ZFSPath: "pool/live@tape-archiver-run-1", RunScoped: true, CreatedAt: time.Date(2026, 10, 16, 3, 4, 5, 0, time.UTC)},
		{ZFSPath: "pool/archive@daily"},
	}}

	assert.Equal(t, []string{
		"pool/live@tape-archiver-run-1 (taken by this run at 2026-10-16 03:04:05 UTC; destroyed after it)",
		"pool/archive@daily",
	}, sourceSnapshots(resolved))
}

// TestBackupExplicitSnapshotTakesNoRunSnapshot: a run with no live datasets
// never creates or destroys a snapshot.
func TestBackupExplicitSnapshotTakesNoRunSnapshot(t *testing.T) {
	t.Parallel()

	env := newBackupEnv(t)
	expectReportDeliverSuccess(env)

	env.OnActivity((&ResolveControlActivities{}).ResolveK8sSources, mock.Anything, mock.Anything).
		Return([]ResolvedArchive(nil), nil)
	env.OnActivity((&ResolveDataActivities{}).ResolveAndCheck, mock.Anything, mock.Anything).
		Return([]ResolvedArchive{{Snapshots: []ResolvedSnapshot{{ZFSPath: "pool/a@snap1"}}}}, nil)
	env.OnActivity((&HoldActivities{}).HoldSnapshots, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&HoldActivities{}).ReleaseSnapshots, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity((&PrepareActivities{}).PrepareArchives, mock.Anything, mock.Anything).Return(nil, nil)

	env.ExecuteWorkflow(Backup, validBackupConfig())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	env.AssertActivityNotCalled(t, "CreateRunSnapshots", mock.Anything, mock.Anything)
	env.AssertActivityNotCalled(t, "DestroyRunSnapshots", mock.Anything, mock.Anything)
}
