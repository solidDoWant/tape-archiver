package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	sessionCreationTimeout = 10 * time.Minute
	// teardownTimeout bounds each TeardownSession attempt; teardownCtx also caps
	// the attempts so a wedged unmount cannot retry forever.
	teardownTimeout = 5 * time.Minute
	// teardownUnmountTimeout bounds the per-mount Unmount wait under the
	// WithoutCancel teardown context. It is shorter than teardownTimeout so a
	// wedged ltfs index-write still reaches the Kill fallback and releases the
	// registry mutex within the same attempt.
	teardownUnmountTimeout = 3 * time.Minute
	// writeTimeoutSafetyFactor scales the floor-speed write duration into the
	// per-tape ceiling: room for overhead, but a wedged drive is still killed.
	writeTimeoutSafetyFactor = 2
	// defaultWriteTapeTimeout is the per-tape ceiling when the capacity maps to no
	// known LTO generation (no floor to derive it from).
	defaultWriteTapeTimeout = 48 * time.Hour
	// writeSessionHeadroom covers the serial non-streaming activities around
	// WriteTree (format, finalize, measure, teardown), keeping the session strictly
	// longer than the per-tape ceiling.
	writeSessionHeadroom = 2 * time.Hour
	// writeHealthTimeout bounds MeasureWriteHealth (two sg_logs scrapes).
	writeHealthTimeout = 5 * time.Minute
)

// perTapeWriteTimeout returns the StartToClose ceiling for one tape's Format →
// WriteTree → FinalizeTape chain: capacity ÷ speed-matching floor (the longest
// a legitimate streaming write can take) × writeTimeoutSafetyFactor, or
// defaultWriteTapeTimeout without a known floor. Derived from stable config, so
// replay-deterministic.
func perTapeWriteTimeout(capacityBytes int64) time.Duration {
	floorMBps, known := writeHealthFloor(capacityBytes)
	if !known || capacityBytes <= 0 || floorMBps <= 0 {
		return defaultWriteTapeTimeout
	}

	// floorMBps is a decimal-MB/s rate; floorMBps × bytesPerMB is bytes/s.
	floorSeconds := float64(capacityBytes) / (floorMBps * bytesPerMB)

	return time.Duration(floorSeconds * writeTimeoutSafetyFactor * float64(time.Second))
}

// writeSessionTimeout returns the Write-phase session ExecutionTimeout. Tapes
// in a set write in parallel, so it bounds the per-drive chain, not the sum.
func writeSessionTimeout(capacityBytes int64) time.Duration {
	return perTapeWriteTimeout(capacityBytes) + writeSessionHeadroom
}

// MountRegistry is a process-global registry of live LTFS mounts keyed by
// device path. It bridges WriteTree and FinalizeTape, which the session pins to
// the same data-worker process.
type MountRegistry struct {
	mu     sync.Mutex
	mounts map[string]*mountEntry
}

// mountEntry is one live mount. unmounted records that a FinalizeTape attempt
// already unmounted it, so a retry skips straight to re-reading the captured
// index instead of being misdiagnosed as a lost mount.
type mountEntry struct {
	mount     *ltfs.Mount
	unmounted bool
}

func newMountRegistry() *MountRegistry {
	return &MountRegistry{mounts: make(map[string]*mountEntry)}
}

// Put stores mount under the given device key, replacing any previous entry.
func (r *MountRegistry) Put(device string, mount *ltfs.Mount) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.mounts[device] = &mountEntry{mount: mount}
}

// Get retrieves the mount for device.
func (r *MountRegistry) Get(device string) (*ltfs.Mount, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.mounts[device]
	if !ok {
		return nil, false
	}

	return entry.mount, true
}

func (r *MountRegistry) getEntry(device string) (mount *ltfs.Mount, unmounted bool, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.mounts[device]
	if !ok {
		return nil, false, false
	}

	return entry.mount, entry.unmounted, true
}

// MarkUnmounted records that device's mount has been unmounted, so a
// FinalizeTape retry re-reads the captured index instead of re-driving the tape.
func (r *MountRegistry) MarkUnmounted(device string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, ok := r.mounts[device]; ok {
		entry.unmounted = true
	}
}

// Delete removes the mount for device.
func (r *MountRegistry) Delete(device string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.mounts, device)
}

// Teardown unmounts every live mount, falling back to Kill when Unmount fails,
// and clears the registry; errors are joined. A data-worker restart loses
// in-memory mounts entirely (SPEC §14).
func (r *MountRegistry) Teardown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error

	for device, entry := range r.mounts {
		// Already unmounted by a prior FinalizeTape: nothing left to release.
		if entry.unmounted {
			delete(r.mounts, device)

			continue
		}

		if err := entry.mount.Unmount(ctx); err != nil {
			// os.ErrProcessDone is harmless: FUSE cleans up when its server dies.
			if killErr := entry.mount.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("teardown %s: unmount: %w; kill: %v", device, err, killErr))
			} else {
				errs = append(errs, fmt.Errorf("teardown %s: unmount: %w", device, err))
			}
		}

		delete(r.mounts, device)
	}

	return errors.Join(errs...)
}

// WriteTreeResult is the WriteTree activity's result. A WriteTree completed
// before #409 decodes as the zero value, i.e. an unknown stream duration.
type WriteTreeResult struct {
	// StreamDuration times only the copy of the staged tree to the mounted
	// volume: not the mount, the manifest, or the unmount-time index write.
	StreamDuration time.Duration
}

// FormatInput is the payload for the FormatTape activity.
type FormatInput struct {
	// Device is the SCSI generic node (e.g. /dev/sg0).
	Device string
	// Barcode is used as the LTFS volume name (SPEC §6).
	Barcode tape.Barcode
}

// WriteTreeInput is the payload for the WriteTree activity.
type WriteTreeInput struct {
	Device string
	// Barcode names the LTFS volume and keys the per-tape mount and work dirs.
	Barcode tape.Barcode
	// TapeIndex is the 0-based logical tape index within plan.Tapes.
	TapeIndex int
	CopyIndex int
	// Archives holds the staged slice and PAR2 files for each archive, in plan order.
	Archives []TapeWriteArchive
}

// FinalizeInput is the payload for the FinalizeTape activity.
type FinalizeInput struct {
	Device string
	// Barcode keys the staged LTFS index path.
	Barcode tape.Barcode
}

// TeardownInput is the payload for the TeardownSession activity.
type TeardownInput struct{}

// WriteActivities hosts FormatTape, WriteTree and FinalizeTape, sharing a
// MountRegistry so a mount parked by WriteTree is found by FinalizeTape.
type WriteActivities struct {
	registry   *MountRegistry
	stagingDir string
}

// newWriteActivities returns WriteActivities rooted at stagingDir
// (TAPE_STAGING_DIR).
func newWriteActivities(registry *MountRegistry, stagingDir string) *WriteActivities {
	return &WriteActivities{registry: registry, stagingDir: stagingDir}
}

// FormatTape formats the loaded tape with mkltfs, naming the volume after its
// barcode (SPEC §6). Single attempt: mkltfs is destructive and non-idempotent.
func (a *WriteActivities) FormatTape(ctx context.Context, input FormatInput) error {
	if err := ltfs.NewVolume(input.Device).Format(ctx, input.Barcode); err != nil {
		return err
	}

	slog.InfoContext(ctx, "write: formatted tape with a fresh LTFS volume",
		"barcode", input.Barcode, "device", input.Device)

	return nil
}

// WriteTree mounts the formatted tape, copies the staged archive tree to it
// (SPEC §6), writes the manifest last, and parks the mount for FinalizeTape.
// Single attempt: mounting is non-idempotent.
//
// The copy is a pure disk→tape stream with no checksumming in the write window
// (SPEC §14); the manifest uses the SHA-256s precomputed during Prepare. Each
// drive reads independently and the ZFS ARC coalesces parallel identical
// streams, so there is no shared fan-out buffer; revisit only if a benchmark
// shows a drive starving below the speed-matching floor.
func (a *WriteActivities) WriteTree(ctx context.Context, input WriteTreeInput) (WriteTreeResult, error) {
	mountDir := filepath.Join(a.stagingDir, "mounts", string(input.Barcode))
	workDir := filepath.Join(a.stagingDir, "work", string(input.Barcode))

	mount, err := ltfs.NewVolume(input.Device).Mount(ctx, mountDir, workDir)
	if err != nil {
		return WriteTreeResult{}, fmt.Errorf("mount LTFS volume on %s: %w", input.Device, err)
	}

	a.registry.Put(input.Device, mount)

	writtenBytes := tapeWrittenBytes(input.Archives)

	slog.InfoContext(ctx, "write: mounted LTFS volume; streaming staged tree to tape",
		"barcode", input.Barcode, "device", input.Device, "archives", len(input.Archives), "bytes", writtenBytes)

	copyStart := time.Now()

	if err := copyTape(ctx, mount.Mountpoint(), input.Archives); err != nil {
		return WriteTreeResult{}, fmt.Errorf("copy staged tree to tape %s: %w", input.Barcode, err)
	}

	result := WriteTreeResult{StreamDuration: time.Since(copyStart)}

	manifest := buildManifest(input.Barcode, input.TapeIndex, input.CopyIndex, input.Archives)
	if err := writeManifest(mount.Mountpoint(), manifest); err != nil {
		return WriteTreeResult{}, fmt.Errorf("write manifest to tape %s: %w", input.Barcode, err)
	}

	slog.InfoContext(ctx, "write: staged tree and per-tape manifest written to tape",
		"barcode", input.Barcode, "device", input.Device, "archives", len(input.Archives), "bytes", writtenBytes,
		"streamDuration", result.StreamDuration)

	return result, nil
}

// FinalizeTape unmounts the live mount (triggering the single deferred index
// write, SPEC §14), then stages the captured index under the run's staging dir
// and returns its path, so the multi-MB index never enters an activity payload
// (#221).
//
// It is retriable: a retry after a successful Unmount re-reads the captured
// index instead of re-driving the tape. Only a data-worker restart loses the
// entry; that tape is re-written next run (SPEC §14).
func (a *WriteActivities) FinalizeTape(ctx context.Context, input FinalizeInput) (string, error) {
	mount, unmounted, ok := a.registry.getEntry(input.Device)
	if !ok {
		// A missing entry can only mean a data-worker restart wiped the registry; no
		// retry can recreate it, so fail fast.
		return "", temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("finalize %s: no live mount in registry (data-worker restarted between write and finalize?)", input.Device),
			"mount-lost",
			nil,
		)
	}

	// On a retry after a successful Unmount, skip straight to re-reading the
	// captured index.
	if !unmounted {
		if err := mount.Unmount(ctx); err != nil {
			// Keep the entry so a retry re-attempts the Unmount.
			return "", fmt.Errorf("unmount LTFS volume on %s: %w", input.Device, err)
		}

		a.registry.MarkUnmounted(input.Device)
	}

	index, err := mount.ReadIndex(ctx)
	if err != nil {
		// Keep the entry (marked unmounted): deleting it would send the retry down the
		// mount-lost branch and re-drive a correctly finalized tape (SPEC §14).
		return "", fmt.Errorf("read captured LTFS index for %s: %w", input.Device, err)
	}

	// Stage before deleting the entry, so a failed stage is retried rather than
	// treated as a lost mount.
	indexPath, err := a.stageIndex(input.Barcode, index)
	if err != nil {
		return "", fmt.Errorf("stage captured LTFS index for %s: %w", input.Device, err)
	}

	a.registry.Delete(input.Device)

	slog.InfoContext(ctx, "write: finalized tape; LTFS index written to tape and captured to disk",
		"barcode", input.Barcode, "device", input.Device, "indexPath", indexPath)

	return indexPath, nil
}

// stageIndex writes the captured LTFS index to a deterministic barcode-keyed
// path under the staging dir (a barcode is in at most one run at a time), for
// the Report phase to read back on the same data worker (#221).
func (a *WriteActivities) stageIndex(barcode tape.Barcode, index []byte) (string, error) {
	dir := filepath.Join(a.stagingDir, "indexes")

	if err := os.MkdirAll(dir, stagingDirPerm); err != nil {
		return "", fmt.Errorf("create index staging directory %s: %w", dir, err)
	}

	path := filepath.Join(dir, string(barcode)+".xml")
	if err := os.WriteFile(path, index, 0o644); err != nil {
		return "", fmt.Errorf("write staged index %s: %w", path, err)
	}

	return path, nil
}

// TeardownActivities hosts TeardownSession.
type TeardownActivities struct {
	registry *MountRegistry
}

func newTeardownActivities(registry *MountRegistry) *TeardownActivities {
	return &TeardownActivities{registry: registry}
}

// TeardownSession unmounts every live LTFS mount the session still owns. It is
// deferred from writePhase so it runs on failure and cancellation too.
func (a *TeardownActivities) TeardownSession(ctx context.Context, _ TeardownInput) error {
	// Detach from cancellation (that is often why we are here), then re-arm a
	// deadline: without one a wedged index write would block Unmount forever
	// holding the registry mutex, and the Kill fallback would never run.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownUnmountTimeout)
	defer cancel()

	return a.registry.Teardown(ctx)
}

// validateBarcodes enforces that every tape in a drive-set has a non-empty,
// unique barcode (SPEC §4.3). Mount and work dirs are keyed solely on barcode,
// so an empty (unlabeled blank) or duplicate one would make parallel WriteTrees
// cross-write trees and read back the wrong index. It reports every offender in
// one joined error.
func validateBarcodes(loaded []LoadedTape) error {
	byBarcode := make(map[tape.Barcode][]int, len(loaded))
	order := make([]tape.Barcode, 0, len(loaded))

	var errs []error

	for _, lt := range loaded {
		if lt.Barcode == "" {
			errs = append(errs, fmt.Errorf("drive %d: empty barcode (unlabeled tape cannot key its LTFS mountpoint)", lt.DriveIndex))

			continue
		}

		if _, seen := byBarcode[lt.Barcode]; !seen {
			order = append(order, lt.Barcode)
		}

		byBarcode[lt.Barcode] = append(byBarcode[lt.Barcode], lt.DriveIndex)
	}

	for _, barcode := range order {
		if drives := byBarcode[barcode]; len(drives) > 1 {
			errs = append(errs, fmt.Errorf("duplicate barcode %q on drives %v", barcode, drives))
		}
	}

	return errors.Join(errs...)
}

// writePhase runs the Write phase (SPEC §4.3) for one drive-set in a Temporal
// session, writing all tapes in parallel (one coroutine per drive). It returns
// the written and failed tapes separately so the caller keeps the good ones and
// re-drives only the failures; the error is reserved for orchestration faults
// that touched no tape.
func writePhase(ctx workflow.Context, cfg config.Config, state *runState, loaded []LoadedTape) ([]WrittenTape, []failedTape, error) {
	// Fails before any format or mount. Reloading the same tapes can never clear
	// this, so it is an orchestration fault rather than an operator pause.
	if err := validateBarcodes(loaded); err != nil {
		return nil, nil, fmt.Errorf("write phase: %w", err)
	}

	// The session must be created on the data queue (tape hardware, session
	// worker, and MountRegistry live there); otherwise it defaults to the control
	// queue, whose worker has no session support, and the run stalls.
	perTapeTimeout := perTapeWriteTimeout(cfg.Library.TapeCapacityBytes)

	sessionBaseCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: perTapeTimeout,
	})

	sessionCtx, err := workflow.CreateSession(sessionBaseCtx, &workflow.SessionOptions{
		CreationTimeout:  sessionCreationTimeout,
		ExecutionTimeout: writeSessionTimeout(cfg.Library.TapeCapacityBytes),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create Write phase session: %w", err)
	}

	defer func() {
		// A disconnected context still schedules teardown when the workflow is being
		// cancelled (the case this defer exists for); it keeps the session binding, so
		// it lands on the worker holding the MountRegistry.
		disconnected, cancel := workflow.NewDisconnectedContext(sessionCtx)
		defer cancel()

		// The server default is unlimited retries; a wedged unmount would hang this
		// .Get forever and notifyFailure would never fire (#152).
		teardownCtx := workflow.WithActivityOptions(disconnected, workflow.ActivityOptions{
			TaskQueue:           DataTaskQueue,
			StartToCloseTimeout: teardownTimeout,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
		})

		var teardown *TeardownActivities
		// Best-effort: do not mask the original phase error.
		_ = workflow.ExecuteActivity(teardownCtx, teardown.TeardownSession, TeardownInput{}).Get(teardownCtx, nil)

		workflow.CompleteSession(sessionCtx)
	}()

	if len(loaded) == 0 {
		return nil, nil, nil
	}

	// Non-idempotent: mkltfs is destructive and a second mount leaves a stale
	// registry entry.
	noRetryOpts := workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: perTapeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	// FinalizeTape retries safely (see FinalizeTape); bound the attempts so a
	// persistent Unmount failure surfaces promptly.
	retryOpts := workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: perTapeTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	}

	type driveResult struct {
		loadedIdx   int
		indexPath   string
		measurement TapeMeasurement
		err         error
	}

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

			// Throughput is timed around the streaming copy only (SPEC §14): the
			// mount and unmount-time index write are fixed overheads that would
			// swamp a small write.
			var written WriteTreeResult
			if err := workflow.ExecuteActivity(noRetryCtx, acts.WriteTree, WriteTreeInput{
				Device:    lt.SGDevice,
				Barcode:   lt.Barcode,
				TapeIndex: lt.TapeIndex,
				CopyIndex: lt.CopyIndex,
				Archives:  archives,
			}).Get(noRetryCtx, &written); err != nil {
				res.err = fmt.Errorf("drive %d: write tree: %w", lt.DriveIndex, err)
				ch.Send(gctx, res)

				return
			}

			var indexPath string
			if err := workflow.ExecuteActivity(retryCtx, acts.FinalizeTape, FinalizeInput{
				Device:  lt.SGDevice,
				Barcode: lt.Barcode,
			}).Get(retryCtx, &indexPath); err != nil {
				res.err = fmt.Errorf("drive %d: finalize: %w", lt.DriveIndex, err)
				ch.Send(gctx, res)

				return
			}

			res.indexPath = indexPath
			// Measure while still loaded (the only time the medium's generation can be
			// read). Observational only (SPEC §2 principle 2): failures never fail the run.
			res.measurement = measureWriteHealth(gctx, lt, archives, written.StreamDuration, floorMBps, floorKnown)

			ch.Send(gctx, res)
		})
	}

	// Partial failure is not fatal: the caller ejects the successes and pauses on
	// the failures.
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
				IndexXMLPath:      res.indexPath,
				WriteHealth:       res.measurement.Health,
				MediaGeneration:   res.measurement.MediaGeneration,
				OverwroteNonBlank: lt.OverwroteNonBlank,
			})
		}
	}

	return written, failed, nil
}

// measureWriteHealth runs MeasureWriteHealth in the session (the host that
// wrote the tape). Write-health never gates a run (SPEC §2 principle 2, §14),
// so a failure is logged and reported as unmeasured.
func measureWriteHealth(gctx workflow.Context, lt LoadedTape, archives []TapeWriteArchive, elapsed time.Duration, floorMBps float64, floorKnown bool) TapeMeasurement {
	// One attempt: the server default retries forever, and a hanging scrape would
	// otherwise stall a finished write phase until the session timeout.
	healthCtx := workflow.WithActivityOptions(gctx, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: writeHealthTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})

	var acts *WriteHealthActivities

	var measurement TapeMeasurement
	if err := workflow.ExecuteActivity(healthCtx, acts.MeasureWriteHealth, MeasureWriteHealthInput{
		Device:       lt.SGDevice,
		Barcode:      lt.Barcode,
		BytesWritten: tapeWrittenBytes(archives),
		Elapsed:      elapsed,
		FloorMBps:    floorMBps,
		FloorKnown:   floorKnown,
	}).Get(healthCtx, &measurement); err != nil {
		workflow.GetLogger(gctx).Warn("tape measurement failed; tape recorded without it",
			"drive", lt.DriveIndex, "barcode", lt.Barcode, "error", err)

		return TapeMeasurement{}
	}

	return measurement
}

// tapeWrittenBytes sums the slices and PAR2 files copied inside the write
// window, the numerator of the sustained write throughput.
func tapeWrittenBytes(archives []TapeWriteArchive) int64 {
	var total int64

	for _, archive := range archives {
		for _, slice := range archive.Slices {
			total += slice.SizeBytes
		}

		for _, par2 := range archive.PAR2Files {
			total += par2.SizeBytes
		}
	}

	return total
}
