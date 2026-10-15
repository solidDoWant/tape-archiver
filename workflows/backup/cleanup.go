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
// path; left behind it grows the staging dataset by a full backup per run. Only
// the report and, when burning, the recovery ISO and its manifest survive: the
// ISO is the master operators verify burned discs against annually (SPEC §10).
// Every path touched is derived from the run itself (SPEC §4.2).

const (
	// cleanupTimeout is generous so only a wedged filesystem hits it; removal
	// is metadata work.
	cleanupTimeout = 1 * time.Hour

	// cleanupMaxAttempts is bounded because cleanup is deferred: unbounded
	// retries of a persistent failure would hold the run open forever.
	cleanupMaxAttempts = 3

	cleanupUnconfiguredErrorType = "CleanupUnconfigured"
)

// retainedRunArtifacts survive cleanup; everything else in the run directory is
// removed. A retain-list (not a delete-list) also sweeps the arbitrary partial
// tree a run dying mid-Prepare leaves behind.
var retainedRunArtifacts = []string{
	reportFileName,
	isoFileName,
	discManifestFileName,
}

// CleanupInput is the staging cleanup payload. The run directory is derived from
// activity info, so no path travels in the payload.
type CleanupInput struct {
	// Barcodes are the tapes this run loaded; their staging entries are keyed by
	// barcode, not run id, so they must be named explicitly.
	Barcodes []string
}

// CleanupActivities hosts the data-side staging cleanup activity.
type CleanupActivities struct {
	stagingRoot string
}

func newCleanupActivities(stagingRoot string) *CleanupActivities {
	return &CleanupActivities{stagingRoot: stagingRoot}
}

// CleanupStaging removes the run's staged bulk data except the retained recovery
// artifacts, plus its per-barcode staging entries. It is idempotent and reports
// every failed removal in one joined error.
func (a *CleanupActivities) CleanupStaging(ctx context.Context, input CleanupInput) error {
	if a.stagingRoot == "" {
		// Guessing a root would recursively remove a path relative to the CWD.
		// Non-retryable: misconfiguration recurs on every attempt.
		return temporal.NewNonRetryableApplicationError(
			"staging directory is not configured (set TAPE_STAGING_DIR on the data worker)",
			cleanupUnconfiguredErrorType, nil)
	}

	runID := activity.GetInfo(ctx).WorkflowExecution.RunID
	if runID == "" {
		// An empty run id would make the staging root itself the run directory,
		// sweeping every run's retained artifacts.
		return temporal.NewNonRetryableApplicationError(
			"cannot derive the run staging directory: activity info carries no run id",
			cleanupUnconfiguredErrorType, nil)
	}

	return a.cleanup(ctx, filepath.Join(a.stagingRoot, runID), input.Barcodes)
}

// cleanup removes runDir's bulk contents and the barcode-keyed staging entries.
func (a *CleanupActivities) cleanup(ctx context.Context, runDir string, barcodes []string) error {
	removed, errs := removeRunStagingContents(runDir)

	for _, barcode := range barcodes {
		if barcode == "" {
			// Joining "" would target the parent directories themselves.
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

// removeRunStagingContents removes every non-retained entry under runDir. An
// absent runDir is not an error.
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

// barcodeStagingPaths returns a tape's barcode-keyed staging-root paths: LTFS
// mountpoint, work directory, and staged index (session.go).
func barcodeStagingPaths(stagingRoot, barcode string) []string {
	return []string{
		filepath.Join(stagingRoot, "mounts", barcode),
		filepath.Join(stagingRoot, "work", barcode),
		filepath.Join(stagingRoot, "indexes", barcode+".xml"),
	}
}

// cleanupBarcodes returns the distinct barcodes the run mounted, in first-seen
// order. It reads mountedBarcodes, not state.loaded (only the in-flight set), so
// earlier drive-sets are cleaned too.
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

// cleanupStaging reclaims the run's staged data on a disconnected context, so it
// also fires on cancellation. It logs but never returns an error, so a failed
// cleanup cannot mask the run's outcome. Running at workflow return guarantees
// every phase has read its artifacts and every LTFS volume is unmounted first.
func cleanupStaging(ctx workflow.Context, state *runState) {
	input := CleanupInput{Barcodes: cleanupBarcodes(state)}

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
