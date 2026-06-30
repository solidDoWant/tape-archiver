package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/ltfs"
	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// Session execution timeouts for the Write phase.
const (
	// sessionCreationTimeout is how long workflow.CreateSession waits for a
	// data worker to accept the session before failing.
	sessionCreationTimeout = 10 * time.Minute
	// sessionExecutionTimeout bounds the entire Write phase session — the sum
	// of all Format + WriteTree + Finalize activities across all tape copies.
	// At LTO speeds a single tape can take hours; 24 h is a safe ceiling.
	sessionExecutionTimeout = 24 * time.Hour
	// teardownTimeout bounds TeardownSession: it only needs to unmount/kill
	// any live mounts, which should complete in well under a minute.
	teardownTimeout = 5 * time.Minute
)

// MountRegistry is a concurrency-safe, process-global registry of live LTFS
// mounts. WriteTree parks a mount here after the volume is mounted so
// FinalizeTape can retrieve it in a later activity execution, even though the
// two activity calls run in separate goroutines. Sessions pin both activities
// to the same data-worker process, making the in-process map safe to use as
// the bridge.
//
// Keys are device paths (e.g. /dev/sg0); one entry per tape drive in flight.
type MountRegistry struct {
	mu     sync.Mutex
	mounts map[string]*ltfs.Mount
}

// newMountRegistry returns an empty MountRegistry.
func newMountRegistry() *MountRegistry {
	return &MountRegistry{mounts: make(map[string]*ltfs.Mount)}
}

// Put stores mount under the given device key, replacing any previous entry.
func (r *MountRegistry) Put(device string, mount *ltfs.Mount) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.mounts[device] = mount
}

// Get retrieves the mount for the given device key. The boolean is false if no
// mount is registered for that device.
func (r *MountRegistry) Get(device string) (*ltfs.Mount, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	m, ok := r.mounts[device]

	return m, ok
}

// Delete removes the mount for the given device key. It is a no-op if the key
// is not present.
func (r *MountRegistry) Delete(device string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.mounts, device)
}

// Teardown unmounts every live mount in the registry and clears it. If
// Unmount returns an error (e.g. the mount is already gone or ctx was
// cancelled), it falls back to Kill so the ltfs process does not linger.
// Teardown continues past individual failures so all mounts are attempted;
// errors from all entries are joined and returned so the caller can surface
// them (e.g. in the Temporal event history via TeardownSession). A data-worker
// restart loses in-memory mounts entirely; that window is documented in
// SPEC §14.
func (r *MountRegistry) Teardown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error

	for device, mount := range r.mounts {
		if err := mount.Unmount(ctx); err != nil {
			// Unmount failed; forcibly kill the ltfs process so it does not
			// linger. os.ErrProcessDone means the process already exited on
			// its own (harmless — the FUSE kernel driver auto-cleans the
			// mount when its server process dies); any other Kill error is
			// included in the returned error.
			if killErr := mount.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("teardown %s: unmount: %w; kill: %v", device, err, killErr))
			} else {
				errs = append(errs, fmt.Errorf("teardown %s: unmount: %w", device, err))
			}
		}

		delete(r.mounts, device)
	}

	return errors.Join(errs...)
}

// FormatInput is the payload for the FormatTape activity.
type FormatInput struct {
	// Device is the SCSI generic node the LTFS sg backend operates on
	// (e.g. /dev/sg0).
	Device string
	// Barcode is the tape's barcode, used as the LTFS volume name (SPEC §6).
	Barcode tape.Barcode
}

// WriteTreeInput is the payload for the WriteTree activity.
type WriteTreeInput struct {
	// Device is the SCSI generic node for the drive holding the formatted tape.
	Device string
	// MountDir is the directory the LTFS FUSE volume is mounted under.
	MountDir string
	// WorkDir is LTFS's work directory; the captured index XML is written here
	// at unmount by FinalizeTape.
	WorkDir string
}

// FinalizeInput is the payload for the FinalizeTape activity.
type FinalizeInput struct {
	// Device identifies the mount to retrieve from the registry.
	Device string
}

// TeardownInput is the payload for the TeardownSession activity.
type TeardownInput struct{}

// WriteActivities hosts the LTFS write activities: FormatTape, WriteTree, and
// FinalizeTape. They share a MountRegistry so WriteTree can park a live mount
// that FinalizeTape retrieves in a later activity execution. Sessions ensure
// both activities run on the same data-worker process, keeping the in-process
// registry bridge valid.
type WriteActivities struct {
	registry *MountRegistry
}

// newWriteActivities returns WriteActivities backed by the given registry.
func newWriteActivities(registry *MountRegistry) *WriteActivities {
	return &WriteActivities{registry: registry}
}

// FormatTape formats the loaded tape with mkltfs, setting its LTFS volume name
// to the tape's barcode (SPEC §6). It is the first activity in the Write
// phase's Format → WriteTree → FinalizeTape sequence.
//
// MaximumAttempts is 1: mkltfs is destructive and non-idempotent. A failure
// here fails the Write phase without retry.
func (a *WriteActivities) FormatTape(ctx context.Context, input FormatInput) error {
	return ltfs.NewVolume(input.Device).Format(ctx, input.Barcode)
}

// WriteTree mounts the LTFS volume on the formatted tape and parks the live
// mount in the registry so FinalizeTape can retrieve it. Files written under
// the mountpoint persist on tape once FinalizeTape unmounts and flushes the
// index.
//
// MaximumAttempts is 1: mounting a formatted tape is non-idempotent (a second
// mount would fail or produce a stale registry entry). A failure here fails
// the Write phase without retry.
//
// Note: the actual tree copy (staging archive slices → mountpoint) is
// implemented by #54. WriteTree in this issue establishes the session model
// and the registry bridge; the copy loop is a TODO for the next sub-issue.
func (a *WriteActivities) WriteTree(ctx context.Context, input WriteTreeInput) error {
	mount, err := ltfs.NewVolume(input.Device).Mount(ctx, input.MountDir, input.WorkDir)
	if err != nil {
		return fmt.Errorf("mount LTFS volume on %s: %w", input.Device, err)
	}

	a.registry.Put(input.Device, mount)

	// TODO (#54): copy staged archive slices and PAR2 recovery files into
	// mount.Mountpoint() here.

	return nil
}

// FinalizeTape unmounts the live LTFS mount from the registry, triggering the
// single deferred index write (SPEC §14), then reads the captured index back
// from the work directory. It removes the mount from the registry on success.
//
// FinalizeTape is safely retriable because it checks ctx.Err() at entry: a
// cancelled context (e.g. from a prior failed attempt) causes an early return
// without touching the mount, leaving it alive in the registry for the next
// attempt. A data-worker restart between WriteTree and FinalizeTape loses the
// in-memory mount — that tape is re-written on the next run (SPEC §14).
func (a *WriteActivities) FinalizeTape(ctx context.Context, input FinalizeInput) ([]byte, error) {
	// Early-exit on a cancelled context so the mount is untouched and remains
	// in the registry for the retry. This is what makes FinalizeTape retriable
	// without re-running WriteTree.
	if err := ctx.Err(); err != nil {
		return nil, temporal.NewApplicationError(
			fmt.Sprintf("finalize %s: context cancelled before unmount", input.Device),
			"retryable",
			err,
		)
	}

	mount, ok := a.registry.Get(input.Device)
	if !ok {
		return nil, fmt.Errorf("finalize %s: no live mount in registry (was WriteTree skipped?)", input.Device)
	}

	if err := mount.Unmount(ctx); err != nil {
		return nil, fmt.Errorf("unmount LTFS volume on %s: %w", input.Device, err)
	}

	index, err := mount.ReadIndex(ctx)
	if err != nil {
		// Index write succeeded (Unmount returned nil) but reading it back
		// failed. Remove from registry to avoid a stale entry — the mount is
		// gone — but propagate the error so the Write phase knows.
		a.registry.Delete(input.Device)

		return nil, fmt.Errorf("read captured LTFS index for %s: %w", input.Device, err)
	}

	a.registry.Delete(input.Device)

	return index, nil
}

// TeardownActivities hosts the TeardownSession activity, which cleans up any
// live LTFS mounts the session still owns when the Write phase ends — on
// success, failure, or cancellation.
type TeardownActivities struct {
	registry *MountRegistry
}

// newTeardownActivities returns TeardownActivities backed by the given registry.
func newTeardownActivities(registry *MountRegistry) *TeardownActivities {
	return &TeardownActivities{registry: registry}
}

// TeardownSession unmounts and releases every live LTFS mount the session owns.
// It is deferred from writePhase so it runs even when the Write phase fails or
// the workflow is cancelled, preventing orphaned FUSE mounts. If a mount has
// already been cleanly finalized by FinalizeTape, the registry is empty and
// this is a no-op.
func (a *TeardownActivities) TeardownSession(ctx context.Context, _ TeardownInput) error {
	// Detach from the cancellation of the incoming activity context: if the
	// Write phase was cancelled (which is why we are being called), we still
	// need to unmount. Use context.WithoutCancel so the teardown runs to
	// completion regardless of the parent cancellation, bounded by the
	// teardownTimeout the workflow configured on the activity options.
	return a.registry.Teardown(context.WithoutCancel(ctx))
}

// writePhase orchestrates the Write phase (SPEC §4.3 phase 7) inside a
// Temporal session. The session pins all data activities to a single
// data-worker process, keeping the in-process MountRegistry valid across the
// Format → WriteTree → FinalizeTape activity boundary.
//
// The deferred TeardownSession activity runs on the same worker (within the
// session) so it can unmount any live mount the session still owns on exit.
//
// The plan iteration and the FormatTape → WriteTree → FinalizeTape calls per
// tape copy are implemented in #54. This scaffold establishes the session model
// and cleanup guarantee that #54 builds on.
func writePhase(ctx workflow.Context, _ config.Config, _ *runState) error {
	sessionCtx, err := workflow.CreateSession(ctx, &workflow.SessionOptions{
		CreationTimeout:  sessionCreationTimeout,
		ExecutionTimeout: sessionExecutionTimeout,
	})
	if err != nil {
		return fmt.Errorf("create Write phase session: %w", err)
	}

	defer func() {
		teardownCtx := workflow.WithActivityOptions(sessionCtx, workflow.ActivityOptions{
			TaskQueue:           DataTaskQueue,
			StartToCloseTimeout: teardownTimeout,
		})

		var teardown *TeardownActivities
		// Best-effort: ignore teardown errors so the original phase error is
		// not masked.
		_ = workflow.ExecuteActivity(teardownCtx, teardown.TeardownSession, TeardownInput{}).Get(teardownCtx, nil)

		workflow.CompleteSession(sessionCtx)
	}()

	// TODO (#54): iterate state.plan.Tapes × state.plan.Copies; for each copy:
	//   1. Dispatch FormatTape within the session.
	//   2. Dispatch WriteTree within the session (parks mount in registry).
	//   3. Dispatch FinalizeTape within the session (retriable; uses registry).

	return nil
}
