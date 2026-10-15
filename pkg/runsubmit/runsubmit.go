// Package runsubmit submits backup runs to Temporal, shared by tapectl and
// the web API so they cannot drift on dry-run semantics or conflict handling.
package runsubmit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/workflows/backup"
)

// mhvtl device environment variables. All are required for a dry-run: there is
// no default because the fallback nodes are identical to the real library's.
const (
	MHVTLChangerEnv = "MHVTL_CHANGER_DEV"
	MHVTLDrive0Env  = "MHVTL_DRIVE0_DEV"
	MHVTLDrive1Env  = "MHVTL_DRIVE1_DEV"
)

// MissingMHVTLEnvError is returned by ApplyDryRun when mhvtl device variables
// are unset.
type MissingMHVTLEnvError struct {
	Missing []string
}

func (e *MissingMHVTLEnvError) Error() string {
	return fmt.Sprintf("dry-run requires the mhvtl virtual-library device(s) to be named "+
		"explicitly, but %s %s unset; set them to the mhvtl nodes (the dev shell's `mhvtl-up` "+
		"exports these) so a dry-run never targets real hardware",
		strings.Join(e.Missing, ", "), pluralIsAre(len(e.Missing)))
}

// ApplyDryRun redirects the changer and drives to mhvtl, disables optical
// burning, and re-validates cfg. The config carries no dry-run marker the
// worker could honor, so this is the only safety net against touching real
// hardware: it fails if any mhvtl variable is unset, and drops optical burn
// because mhvtl has no virtual burner (an advisory goes to warnOut).
func ApplyDryRun(cfg *config.Config, getenv func(string) string, warnOut io.Writer) error {
	changer := getenv(MHVTLChangerEnv)
	drive0 := getenv(MHVTLDrive0Env)
	drive1 := getenv(MHVTLDrive1Env)

	var missing []string

	if changer == "" {
		missing = append(missing, MHVTLChangerEnv)
	}

	if drive0 == "" {
		missing = append(missing, MHVTLDrive0Env)
	}

	if drive1 == "" {
		missing = append(missing, MHVTLDrive1Env)
	}

	if len(missing) != 0 {
		return &MissingMHVTLEnvError{Missing: missing}
	}

	// Record the real changer so the worker can mirror its slots into mhvtl;
	// this is the one place both devices are known. A client-supplied value is
	// never trusted.
	cfg.Library.MirrorSourceChanger = ""
	if cfg.Library.Changer != "" && cfg.Library.Changer != changer {
		cfg.Library.MirrorSourceChanger = cfg.Library.Changer
	}

	cfg.Library.Changer = changer
	cfg.Library.Drives = []string{drive0, drive1}

	if cfg.Delivery.OpticalBurn.Enabled() {
		// Best-effort; pkg/runsapi passes io.Discard.
		_, _ = fmt.Fprintln(warnOut, "tapectl: dry-run: optical burning disabled — mhvtl provides no "+
			"virtual optical burner, so delivery.opticalBurn is skipped and the real burner is "+
			"never probed, blanked, or written")
	}

	cfg.Delivery.OpticalBurn = nil

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config after dry-run override: %w", err)
	}

	return nil
}

func pluralIsAre(count int) string {
	if count == 1 {
		return "is"
	}

	return "are"
}

// TemporalClient is the subset of client.Client needed to submit a backup run.
type TemporalClient interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

var _ TemporalClient = client.Client(nil)

// MemoKeyDryRun is the memo field recording whether a run was a dry run.
const MemoKeyDryRun = backup.MemoKeyDryRun

// StartOptions returns the options every backup submission must use: the
// singleton workflow ID, failing if a run is already in progress (SPEC §4.2).
func StartOptions(dryRun bool) client.StartWorkflowOptions {
	return client.StartWorkflowOptions{
		ID:        backup.WorkflowID,
		TaskQueue: backup.TaskQueue,
		Memo:      map[string]interface{}{MemoKeyDryRun: dryRun},
		// Surface the conflict as an error rather than returning the running run.
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}
}

// Submit starts the backup workflow with cfg under StartOptions, translating
// errors via TranslateSubmitError. A production run's MirrorSourceChanger is
// cleared: only ApplyDryRun may set it, and the mirror moves tapes in the target
// library (issue #382).
func Submit(ctx context.Context, temporalClient TemporalClient, cfg *config.Config, dryRun bool) (client.WorkflowRun, error) {
	if !dryRun {
		cfg.Library.MirrorSourceChanger = ""
	}

	run, err := temporalClient.ExecuteWorkflow(ctx, StartOptions(dryRun), backup.WorkflowType, cfg)
	if err != nil {
		return nil, TranslateSubmitError(err)
	}

	return run, nil
}

// TranslateSubmitError converts an ExecuteWorkflow error into an
// operator-facing message, naming the in-progress run on a singleton conflict.
func TranslateSubmitError(err error) error {
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &alreadyStarted) {
		return &submitConflictError{
			msg: fmt.Sprintf("a backup run is already in progress (workflow ID %q, run ID %s); "+
				"backup runs are mutually exclusive — wait for it to finish or inspect it with `tapectl status`",
				backup.WorkflowID, alreadyStarted.RunId),
			err: err,
		}
	}

	return fmt.Errorf("submit backup workflow: %w", err)
}

// submitConflictError keeps Error() free of raw Temporal text while Unwrap()
// still exposes the original error.
type submitConflictError struct {
	msg string
	err error
}

func (e *submitConflictError) Error() string { return e.msg }

func (e *submitConflictError) Unwrap() error { return e.err }
