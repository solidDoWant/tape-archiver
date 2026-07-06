package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// writeTapeTimeout bounds a single tape's Format → WriteTree → FinalizeTape
	// chain. Streaming terabytes to LTO tape takes many hours; 24 h per tape is
	// a generous but realistic ceiling.
	writeTapeTimeout = 24 * time.Hour
	// writeHealthTimeout bounds the post-write MeasureWriteHealth activity. It only
	// scrapes two SCSI log pages via sg_logs, which completes in seconds; a few
	// minutes is a generous ceiling that still bounds a hang.
	writeHealthTimeout = 5 * time.Minute
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
	// Barcode is the tape's barcode; it names the LTFS volume and is used to
	// derive the per-tape mount and work directories under the staging root.
	Barcode tape.Barcode
	// TapeIndex is the 0-based logical tape index within plan.Tapes; it selects
	// which archives to copy to this tape.
	TapeIndex int
	// CopyIndex is the 0-based copy number among the copies of this logical tape.
	CopyIndex int
	// Archives holds the staged slice and PAR2 files for each archive on this
	// tape, in plan order.
	Archives []TapeWriteArchive
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
	registry   *MountRegistry
	stagingDir string
}

// newWriteActivities returns WriteActivities backed by the given registry.
// stagingDir is the data worker's staging root (TAPE_STAGING_DIR); per-tape
// mount and work directories are created under it.
func newWriteActivities(registry *MountRegistry, stagingDir string) *WriteActivities {
	return &WriteActivities{registry: registry, stagingDir: stagingDir}
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

// WriteTree mounts the LTFS volume on the formatted tape, copies the staged
// archive tree to the mountpoint (archives/NNN-<label>/ per archive, SPEC §6), writes
// the per-tape manifest last, and parks the live mount in the registry so
// FinalizeTape can retrieve it.
//
// Per-tape mount and work directories are derived from the staging root and the
// tape's barcode; they persist across activity boundaries (the session pins all
// activities to the same worker process).
//
// MaximumAttempts is 1: mounting a formatted tape is non-idempotent (a second
// mount would fail or produce a stale registry entry). A failure here fails
// the Write phase without retry.
//
// The copy is a pure disk→tape stream — no checksumming in the write window
// (SPEC §14). The precomputed SHA-256s from Prepare/GeneratePAR2 populate the
// manifest so a future recoverer can verify files without re-reading every byte.
//
// Independent readers per drive: each drive's WriteTree reads staged files
// independently. The ZFS ARC and sequential read-ahead transparently coalesce
// in-lockstep streams from multiple drives into near-1× physical disk reads, and
// allow a lagging drive to re-read from its own page-cache window when it drifts
// past the coalesced read. This adaptive kernel behaviour eliminates the need for
// a hand-rolled shared cursor or fan-out buffer, which would couple drives and
// risk thrash. If a hardware benchmark ever shows a per-drive shortfall below the
// LTO speed-matching floor, revisit with a static fan-out or application-level
// buffer (SPEC §14); until then the kernel ARC is the right tool.
func (a *WriteActivities) WriteTree(ctx context.Context, input WriteTreeInput) error {
	mountDir := filepath.Join(a.stagingDir, "mounts", string(input.Barcode))
	workDir := filepath.Join(a.stagingDir, "work", string(input.Barcode))

	mount, err := ltfs.NewVolume(input.Device).Mount(ctx, mountDir, workDir)
	if err != nil {
		return fmt.Errorf("mount LTFS volume on %s: %w", input.Device, err)
	}

	a.registry.Put(input.Device, mount)

	if err := copyTape(ctx, mount.Mountpoint(), input.Archives); err != nil {
		return fmt.Errorf("copy staged tree to tape %s: %w", input.Barcode, err)
	}

	manifest := buildManifest(input.Barcode, input.TapeIndex, input.CopyIndex, input.Archives)
	if err := writeManifest(mount.Mountpoint(), manifest); err != nil {
		return fmt.Errorf("write manifest to tape %s: %w", input.Barcode, err)
	}

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
		// A missing mount is terminal, not transient: the only way it is absent
		// here (after WriteTree parked it in the same process) is a data-worker
		// restart that wiped the in-memory registry. No retry can recreate it —
		// that tape is re-written on the next run (SPEC §14). Mark the error
		// non-retryable so the Write phase fails fast instead of retrying the
		// unrecoverable state until the session timeout.
		return nil, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("finalize %s: no live mount in registry (data-worker restarted between write and finalize?)", input.Device),
			"mount-lost",
			nil,
		)
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

// writePhase orchestrates the Write phase (SPEC §4.3 phase 7) for one drive-set
// inside a Temporal session. The session pins all data activities to a single
// data-worker process, keeping the in-process MountRegistry valid across the
// Format → WriteTree → FinalizeTape activity boundary.
//
// All tapes in the set write in parallel — one Temporal coroutine per drive, each
// sequencing Format → WriteTree → Finalize independently, so the set holds at most
// len(Drives) tapes in flight at once (concurrent disk reads bounded by the drive
// count, SPEC §14). This keeps drives decoupled: a slow drive does not gate a fast
// one, and the ZFS ARC provides adaptive read coalescing for byte-identical copies
// without a hand-rolled buffer. runTapePath calls it once per set with that set's
// loaded tapes and appends the returned WrittenTapes to the run's cumulative list.
//
// The deferred TeardownSession activity runs on the same worker (within the
// session) so it can unmount any live mount the session still owns on exit.
//
// It returns the tapes that wrote successfully and, separately, the tapes whose
// Format/WriteTree/FinalizeTape failed — so a partial failure keeps the good
// tapes (the caller ejects and records them) while the caller pauses on and
// re-drives only the failed ones (SPEC §4.3). The returned error is reserved for
// an unrecoverable orchestration fault (e.g. session creation) that touched no
// tape; per-tape write failures come back as failedTape values, not as error.
func writePhase(ctx workflow.Context, cfg config.Config, state *runState, loaded []LoadedTape) ([]WrittenTape, []failedTape, error) {
	// CreateSession pins the session to the task queue in the context's activity
	// options, falling back to the workflow's own queue (control) when none is
	// set. The session must run on the data worker — that is where the tape
	// hardware, the session worker, and the live MountRegistry live — so set the
	// data task queue before creating it. Without this the session is created on
	// the control queue, whose worker has no session support, and the internal
	// session-creation activity is never picked up (the run stalls).
	sessionBaseCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: writeTapeTimeout,
	})

	sessionCtx, err := workflow.CreateSession(sessionBaseCtx, &workflow.SessionOptions{
		CreationTimeout:  sessionCreationTimeout,
		ExecutionTimeout: sessionExecutionTimeout,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create Write phase session: %w", err)
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

	if len(loaded) == 0 {
		return nil, nil, nil
	}

	// Activity options for Format and WriteTree: MaximumAttempts=1 because
	// both are non-idempotent (mkltfs is destructive; mounting a second time
	// produces a stale registry entry).
	noRetryOpts := workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: writeTapeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	// FinalizeTape uses the default retry policy: its ctx.Err() guard makes
	// it safe to retry without re-running WriteTree.
	retryOpts := workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: writeTapeTimeout,
	}

	type driveResult struct {
		loadedIdx int
		indexXML  []byte
		health    WriteHealth
		err       error
	}

	// The speed-matching floor is a property of the tape generation being written,
	// derived once from the configured native capacity (see measureWriteHealth).
	floorMBps, floorKnown := writeHealthFloor(cfg.Library.TapeCapacityBytes)

	ch := workflow.NewBufferedChannel(sessionCtx, len(loaded))

	for i, lt := range loaded {
		i, lt := i, lt

		workflow.Go(sessionCtx, func(gctx workflow.Context) {
			res := driveResult{loadedIdx: i}

			noRetryCtx := workflow.WithActivityOptions(gctx, noRetryOpts)
			retryCtx := workflow.WithActivityOptions(gctx, retryOpts)

			var acts *WriteActivities

			if err := workflow.ExecuteActivity(noRetryCtx, acts.FormatTape, FormatInput{
				Device:  lt.SGDevice,
				Barcode: lt.Barcode,
			}).Get(noRetryCtx, nil); err != nil {
				res.err = fmt.Errorf("drive %d: format: %w", lt.DriveIndex, err)
				ch.Send(gctx, res)

				return
			}

			archives, err := archivesForTape(state, lt.TapeIndex)
			if err != nil {
				res.err = fmt.Errorf("drive %d: assemble archives: %w", lt.DriveIndex, err)
				ch.Send(gctx, res)

				return
			}

			// Start the write-window clock before the copy and stop it after the
			// finalize/unmount so the measured span is exactly WriteTree →
			// FinalizeTape (SPEC §14). workflow.Now is the replay-safe workflow
			// clock, so the elapsed time is deterministic across retries.
			writeStart := workflow.Now(gctx)

			if err := workflow.ExecuteActivity(noRetryCtx, acts.WriteTree, WriteTreeInput{
				Device:    lt.SGDevice,
				Barcode:   lt.Barcode,
				TapeIndex: lt.TapeIndex,
				CopyIndex: lt.CopyIndex,
				Archives:  archives,
			}).Get(noRetryCtx, nil); err != nil {
				res.err = fmt.Errorf("drive %d: write tree: %w", lt.DriveIndex, err)
				ch.Send(gctx, res)

				return
			}

			var indexXML []byte
			if err := workflow.ExecuteActivity(retryCtx, acts.FinalizeTape, FinalizeInput{
				Device: lt.SGDevice,
			}).Get(retryCtx, &indexXML); err != nil {
				res.err = fmt.Errorf("drive %d: finalize: %w", lt.DriveIndex, err)
				ch.Send(gctx, res)

				return
			}

			res.indexXML = indexXML
			// Measure write-health after the window closed (unmount and the
			// deferred index sync have settled). This is observational only
			// (SPEC §2 principle 2): a measurement failure is logged and the tape
			// is still recorded as written — it never fails the run.
			res.health = measureWriteHealth(gctx, lt, archives, workflow.Now(gctx).Sub(writeStart), floorMBps, floorKnown)

			ch.Send(gctx, res)
		})
	}

	// Collect results from all drives, partitioning them into the tapes that wrote
	// successfully and the tapes that failed. A partial failure is not fatal here:
	// the caller ejects and records the successes and pauses on the failures, so
	// the failed tapes come back as failedTape values rather than a joined error.
	var written []WrittenTape

	var failed []failedTape

	for range loaded {
		var res driveResult
		ch.Receive(sessionCtx, &res)

		lt := loaded[res.loadedIdx]

		if res.err != nil {
			failed = append(failed, failedTape{Tape: lt, Err: res.err})
		} else {
			written = append(written, WrittenTape{
				Barcode:           lt.Barcode,
				DriveIndex:        lt.DriveIndex,
				TapeIndex:         lt.TapeIndex,
				CopyIndex:         lt.CopyIndex,
				SourceSlot:        lt.SourceSlot,
				IndexXML:          res.indexXML,
				WriteHealth:       res.health,
				OverwroteNonBlank: lt.OverwroteNonBlank,
			})
		}
	}

	return written, failed, nil
}

// measureWriteHealth runs the observational MeasureWriteHealth activity for one tape
// after its write window closed, returning the verdict. The activity runs in the
// session context so it lands on the same data worker (and host) that wrote the tape,
// where its SCSI generic node lives. Because write-health never gates a run (SPEC §2
// principle 2, §14), a measurement failure is logged and reported as unmeasured
// rather than propagated.
func measureWriteHealth(gctx workflow.Context, lt LoadedTape, archives []TapeWriteArchive, elapsed time.Duration, floorMBps float64, floorKnown bool) WriteHealth {
	healthCtx := workflow.WithActivityOptions(gctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: writeHealthTimeout,
	})

	var acts *WriteHealthActivities

	var health WriteHealth
	if err := workflow.ExecuteActivity(healthCtx, acts.MeasureWriteHealth, MeasureWriteHealthInput{
		Device:      lt.SGDevice,
		Barcode:     lt.Barcode,
		StagedBytes: stagedBytes(archives),
		Elapsed:     elapsed,
		FloorMBps:   floorMBps,
		FloorKnown:  floorKnown,
	}).Get(healthCtx, &health); err != nil {
		workflow.GetLogger(gctx).Warn("write-health measurement failed; tape recorded without it",
			"drive", lt.DriveIndex, "barcode", lt.Barcode, "error", err)

		return WriteHealth{}
	}

	return health
}

// stagedBytes sums the staged archive data on a tape — the slice bytes only, which is
// exactly StagedArchive.SizeBytes (PAR2 recovery bytes are excluded, per issue #70).
// It is the numerator of the sustained write throughput.
func stagedBytes(archives []TapeWriteArchive) int64 {
	var total int64

	for _, archive := range archives {
		for _, slice := range archive.Slices {
			total += slice.SizeBytes
		}
	}

	return total
}
