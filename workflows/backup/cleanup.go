package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// Staging cleanup (SPEC §4.3) reclaims the run's staged bulk data on every exit
// path — success, failure, and cancellation. Prepare stages a complete second copy
// of everything the run archives (age-encrypted slices, plus their PAR2 recovery
// sets) under the staging root so the write window is a pure disk→tape copy
// (SPEC §4.3, §14). Once the run closes those bytes are dead weight: the tapes are
// the system of record and there is no resume-from-staging path, so leaving them
// behind grows the staging dataset by roughly one full backup per run until it
// fills and fails a later run mid-Prepare.
//
// What survives is deliberately small and durable: the PDF report and — when
// optical burning is enabled — the recovery ISO and its disc-content manifest.
// Operators are directed to verify each burned disc annually against that archived
// master ISO (SPEC §10, docs/maintenance.md), so it is the one staged artifact with
// a life beyond the run. Those retained artifacts accumulate across runs by design;
// pruning them is a documented operator task, not run behavior.
//
// No cross-run state is introduced (SPEC §4.2): every path cleanup touches is
// derived from the run itself — its own run-id subdirectory, and the barcodes its
// own drive-sets loaded.

const (
	// cleanupTimeout bounds the staging cleanup activity. Removal is metadata
	// work — ZFS frees the blocks asynchronously — so even a multi-terabyte tree
	// unlinks quickly; the bound is generous purely so a wedged filesystem
	// surfaces as a timeout rather than holding the run open.
	cleanupTimeout = 1 * time.Hour

	// cleanupMaxAttempts bounds the cleanup activity's retries. Cleanup is
	// deferred, so the workflow cannot close until it finishes: with Temporal's
	// unbounded default policy a persistently failing removal (a wedged mount, a
	// read-only filesystem) would retry forever and hold the run open in the
	// operator's run list indefinitely. A few attempts absorb a transient I/O
	// error; past that, leftover staging is observable on disk and manually
	// clearable (docs/maintenance.md) and the run must be allowed to close.
	cleanupMaxAttempts = 3

	// cleanupUnconfiguredErrorType is the application-error type for a cleanup
	// that cannot run because the data worker is misconfigured.
	cleanupUnconfiguredErrorType = "CleanupUnconfigured"
)

// retainedRunArtifacts are the file names within a run's staging directory that
// survive cleanup: the durable recovery artifacts (SPEC §9, §10). Everything else
// under the run directory is bulk staging output and is removed.
//
// The list is intentionally a retain-list rather than an enumeration of what to
// delete. Prepare and Generate PAR2 lay down a nested, run-shaped tree (NNN/
// per archive), and a run that dies partway through Prepare leaves an arbitrary
// prefix of it; deleting only the paths a *successful* run would have produced
// would let that partial tree survive. Retaining a fixed, tiny set and removing
// everything else makes "no staged slice or PAR2 file remains" true by
// construction, whenever the run happened to stop.
var retainedRunArtifacts = []string{
	reportFileName,
	isoFileName,
	discManifestFileName,
}

// CleanupInput is the payload for the staging cleanup activity. The run
// directory is not carried here: the activity derives it from its own activity
// info (the same run id Prepare and Report stage under), so the two cannot drift
// and no filesystem path travels in an activity payload.
type CleanupInput struct {
	// Barcodes are the tapes this run loaded, whose per-tape LTFS mountpoint,
	// work directory, and staged index file are removed from the staging root.
	// Those entries are keyed by barcode rather than by run id, so they cannot be
	// swept by the run-directory pass and must be named explicitly.
	Barcodes []string
}

// CleanupActivities hosts the data-side staging cleanup activity. stagingRoot is
// the directory all runs stage under (operational worker config, SPEC §4.1) — the
// same root Prepare, Write, and Report are constructed with.
type CleanupActivities struct {
	stagingRoot string
}

// newCleanupActivities returns the data-side staging cleanup activity, reclaiming
// staged output beneath stagingRoot.
func newCleanupActivities(stagingRoot string) *CleanupActivities {
	return &CleanupActivities{stagingRoot: stagingRoot}
}

// CleanupStaging removes the run's staged bulk data: everything under its run-id
// staging directory except the durable recovery artifacts, plus the per-barcode
// LTFS mountpoint, work directory, and staged index file the run created at the
// staging root.
//
// It is idempotent — removing an already-absent path is a no-op — so an activity
// retry, or a run that staged nothing at all before failing, both succeed without
// special-casing. It reports every path it could not remove in a single joined
// error rather than stopping at the first, so one stubborn entry never hides the
// rest from the operator reading the log.
func (a *CleanupActivities) CleanupStaging(ctx context.Context, input CleanupInput) error {
	if a.stagingRoot == "" {
		// Without a staging root there is no path to derive, and guessing one
		// would mean recursively removing a run-id-named directory relative to
		// the worker's working directory. Refuse instead — and non-retryably:
		// worker misconfiguration recurs identically on every attempt, and this
		// activity is deferred, so retrying would hold the run open for nothing.
		return temporal.NewNonRetryableApplicationError(
			"staging directory is not configured (set TAPE_STAGING_DIR on the data worker)",
			cleanupUnconfiguredErrorType, nil)
	}

	runID := activity.GetInfo(ctx).WorkflowExecution.RunID
	if runID == "" {
		// A missing run id would make the run directory the staging root itself,
		// which would sweep away every run's retained artifacts at once.
		return temporal.NewNonRetryableApplicationError(
			"cannot derive the run staging directory: activity info carries no run id",
			cleanupUnconfiguredErrorType, nil)
	}

	return a.cleanup(ctx, filepath.Join(a.stagingRoot, runID), input.Barcodes)
}

// cleanup removes runDir's bulk contents and the barcode-keyed staging entries.
// It is split from CleanupStaging so it can be exercised against a real directory
// without an activity context.
func (a *CleanupActivities) cleanup(ctx context.Context, runDir string, barcodes []string) error {
	removed, errs := removeRunStagingContents(runDir)

	for _, barcode := range barcodes {
		if barcode == "" {
			// An unlabeled tape never reached WriteTree (validateBarcodes rejects
			// the drive-set first), so it has no barcode-keyed entries; joining
			// the empty string would target the parent directories themselves.
			continue
		}

		for _, path := range barcodeStagingPaths(a.stagingRoot, barcode) {
			if err := os.RemoveAll(path); err != nil {
				errs = append(errs, fmt.Errorf("remove %q: %w", path, err))

				continue
			}

			removed++
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("clean up staging directory %q: %w", runDir, err)
	}

	slog.InfoContext(ctx, "cleanup: reclaimed the run's staged bulk data",
		"runDir", runDir, "removed", removed, "barcodes", len(barcodes), "retained", retainedRunArtifacts)

	return nil
}

// removeRunStagingContents removes every entry directly under runDir except the
// retained recovery artifacts, returning how many it removed and any removal
// errors. An absent runDir is not an error: a run that failed before Prepare
// staged anything has nothing to clean.
func removeRunStagingContents(runDir string) (int, []error) {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}

		return 0, []error{fmt.Errorf("read run staging directory %q: %w", runDir, err)}
	}

	var (
		removed int
		errs    []error
	)

	for _, entry := range entries {
		if slices.Contains(retainedRunArtifacts, entry.Name()) {
			continue
		}

		path := filepath.Join(runDir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %q: %w", path, err))

			continue
		}

		removed++
	}

	return removed, errs
}

// barcodeStagingPaths returns the staging-root paths keyed on a single tape's
// barcode: the LTFS mountpoint and work directory WriteTree mounts through, and
// the captured index FinalizeTape stages (session.go). They live at the staging
// root rather than under the run directory, so cleanup names them per barcode.
func barcodeStagingPaths(stagingRoot, barcode string) []string {
	return []string{
		filepath.Join(stagingRoot, "mounts", barcode),
		filepath.Join(stagingRoot, "work", barcode),
		filepath.Join(stagingRoot, "indexes", barcode+".xml"),
	}
}

// cleanupBarcodes returns the distinct barcodes the run loaded, in first-seen
// order, as the strings the activity payload carries. It reads the cumulative
// mountedBarcodes rather than state.loaded, which holds only the in-flight
// drive-set: a run spanning several sets would otherwise leave every earlier
// set's mountpoint and work directory behind.
func cleanupBarcodes(state *runState) []string {
	seen := make(map[tape.Barcode]struct{})

	var barcodes []string

	for _, barcode := range state.mountedBarcodes {
		if barcode == "" {
			continue
		}

		if _, ok := seen[barcode]; ok {
			continue
		}

		seen[barcode] = struct{}{}
		barcodes = append(barcodes, string(barcode))
	}

	return barcodes
}

// cleanupStaging reclaims the run's staged bulk data. It runs on a disconnected
// context so the cleanup still fires when the workflow is cancelled (modeled on
// releaseSnapshots), and it logs but never returns a cleanup error: a failed
// cleanup must not mask the run's own outcome — leftover staging is observable on
// disk and manually clearable (docs/maintenance.md).
//
// It is deferred once, *before* the first phase runs, so it fires at workflow
// return on every exit path — including a failure partway through Prepare, which
// is precisely the case that strands the most bytes. Removing a directory that
// was never created is a no-op, so arming it ahead of any staging is safe.
//
// Deferring at workflow return also orders it correctly with no explicit
// sequencing: Report, Burn, and Deliver have already read the staged artifacts by
// then, and writePhase's own deferred TeardownSession has unmounted every LTFS
// volume, so no mountpoint is removed while a FUSE mount is still live.
func cleanupStaging(ctx workflow.Context, state *runState) {
	input := CleanupInput{Barcodes: cleanupBarcodes(state)}

	// A disconnected context is not cancelled when the workflow is, so the staged
	// data is reclaimed even on cancellation (SPEC §4.3).
	disconnected, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()

	actx := workflow.WithActivityOptions(disconnected, workflow.ActivityOptions{
		TaskQueue:           DataTaskQueue,
		StartToCloseTimeout: cleanupTimeout,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: cleanupMaxAttempts},
	})

	var activities *CleanupActivities
	if err := workflow.ExecuteActivity(actx, activities.CleanupStaging, input).Get(actx, nil); err != nil {
		workflow.GetLogger(ctx).Error("failed to clean up the run's staging directory",
			"barcodes", len(input.Barcodes),
			"error", err,
		)
	}
}
