// Package runsubmit is the shared path for submitting a backup run to
// Temporal: the dry-run device override (mhvtl redirection + optical-burn
// disablement) and the fixed submission options/error translation. It is
// imported by both `cmd/tapectl` (`tapectl run [--dry-run]`) and
// `pkg/runsapi` (`POST /api/runs`) so the two front doors to Temporal — CLI
// and browser — can never drift on what "dry run" means or how a
// singleton-conflict is reported (docs/web-ui-design.md §2, §8 item 3).
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

// mhvtl device environment variables. A dry-run points the worker at the mhvtl
// virtual tape library instead of the real library (SPEC §12). There is no
// hardware default: the fallback nodes (`/dev/sch0`, `/dev/nstX`) are
// byte-identical to the real library, so a missing override would silently
// aim a dry-run at real hardware. Instead every variable is required, and a
// dry-run with any of them unset fails fast (see ApplyDryRun).
const (
	MHVTLChangerEnv = "MHVTL_CHANGER_DEV"
	MHVTLDrive0Env  = "MHVTL_DRIVE0_DEV"
	MHVTLDrive1Env  = "MHVTL_DRIVE1_DEV"
)

// ApplyDryRun rewrites the library device targets to the mhvtl virtual library
// so the run exercises virtual hardware instead of the real changer and drives.
// The two mhvtl drives replace whatever drives the config named; the blank
// slots are left untouched, as they are logical positions in the library.
//
// The mhvtl device nodes must be named explicitly via the environment. Because
// the fallback nodes would be indistinguishable from the real library — and the
// devices are opened worker-side while these variables are read client-side, so
// the submitted config carries no dry-run marker the worker could honor — a
// dry-run with any variable unset returns an actionable error and rewrites
// nothing, rather than silently targeting real hardware (CLAUDE.md Hardware and
// Safety; SPEC §12).
//
// ApplyDryRun also disables optical burning. mhvtl provides no virtual optical
// burner, so — unlike the tape library — there is no safe device to redirect to,
// and the submitted config carries no dry-run marker the worker could honor. Left
// in place, delivery.opticalBurn keeps OpticalBurn.Enabled() true and the worker
// would probe, pause on, blank, and irreversibly burn the operator's real burner
// during what is meant to be a hardware-free test. Neutralizing the section
// (rather than refusing the whole config) keeps the tape path — which mhvtl can
// exercise end to end — dry-runnable for configs that also configure burning; the
// run then completes exactly as a no-optical-burn run (burnPhase is a no-op). An
// advisory is written to warnOut so the operator/caller knows burning was skipped
// for the dry-run; pass io.Discard to suppress it (e.g. a server-side caller with
// no place to surface a stderr-shaped advisory).
//
// Finally, ApplyDryRun re-validates cfg: the override changes the device set, so
// the result must satisfy the same invariants (e.g. copies <= drives) as the
// original config. Callers do not need a separate cfg.Validate() call after this
// returns nil.
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
		return fmt.Errorf("dry-run requires the mhvtl virtual-library device(s) to be named "+
			"explicitly, but %s %s unset; set them to the mhvtl nodes (the dev shell's `mhvtl-up` "+
			"exports these) so a dry-run never targets real hardware",
			strings.Join(missing, ", "), pluralIsAre(len(missing)))
	}

	cfg.Library.Changer = changer
	cfg.Library.Drives = []string{drive0, drive1}

	// Disable optical burning: there is no virtual burner to redirect to, so the
	// only safe target is off. Enabled() is nil-safe, so this is a no-op when the
	// section is absent or already disabled.
	if cfg.Delivery.OpticalBurn.Enabled() {
		// Best-effort advisory: a failed write to warnOut must not fail the dry-run.
		_, _ = fmt.Fprintln(warnOut, "dry-run: optical burning disabled — mhvtl provides no "+
			"virtual optical burner, so delivery.opticalBurn is skipped and the real burner is "+
			"never probed, blanked, or written")
	}

	cfg.Delivery.OpticalBurn = nil

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config after dry-run override: %w", err)
	}

	return nil
}

// pluralIsAre returns the correct verb form for the count of missing variables.
func pluralIsAre(count int) string {
	if count == 1 {
		return "is"
	}

	return "are"
}

// TemporalClient is the subset of client.Client needed to submit a backup
// run. Both cmd/tapectl's full client.Client and pkg/runsapi's minimal
// TemporalClient satisfy it, so this package never forces either caller to
// depend on more of the SDK surface than it already does.
type TemporalClient interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// Compile-time assertion that the real Temporal SDK client satisfies
// TemporalClient.
var _ TemporalClient = client.Client(nil)

// StartOptions returns the fixed Temporal StartWorkflowOptions every backup
// run submission must use: the singleton workflow ID on the control task
// queue, with a conflict policy that fails a second submission while one is
// already running rather than queuing or replacing it (SPEC §4.2 — a run is
// a singleton, one data worker on one storage host).
func StartOptions() client.StartWorkflowOptions {
	return client.StartWorkflowOptions{
		ID:        backup.WorkflowID,
		TaskQueue: backup.TaskQueue,
		// Refuse to start a second run while one is already running, but allow a
		// fresh run once the previous one has closed. WorkflowExecutionError-
		// WhenAlreadyStarted makes ExecuteWorkflow return the conflict as an
		// error rather than silently handing back the running run.
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}
}

// Submit starts the backup workflow with cfg as its argument, under the
// fixed singleton StartOptions. Any error — including a singleton conflict —
// is translated via TranslateSubmitError into an operator-facing message
// before being returned.
func Submit(ctx context.Context, temporalClient TemporalClient, cfg *config.Config) (client.WorkflowRun, error) {
	run, err := temporalClient.ExecuteWorkflow(ctx, StartOptions(), backup.WorkflowType, cfg)
	if err != nil {
		return nil, TranslateSubmitError(err)
	}

	return run, nil
}

// TranslateSubmitError converts the error returned by ExecuteWorkflow into an
// operator-facing message. A conflict with an already-running backup (the
// singleton guard firing) is reported as an actionable message naming the
// in-progress run rather than an opaque Temporal error; every other error is
// wrapped verbatim. The original error is preserved via %w in both cases, so
// callers that need to classify the error further (e.g. pkg/runsapi mapping
// it to an HTTP status) can still errors.As through the translated message.
func TranslateSubmitError(err error) error {
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &alreadyStarted) {
		return fmt.Errorf("a backup run is already in progress (workflow ID %q, run ID %s); "+
			"backup runs are mutually exclusive — wait for it to finish or inspect it with `tapectl status`: %w",
			backup.WorkflowID, alreadyStarted.RunId, err)
	}

	return fmt.Errorf("submit backup workflow: %w", err)
}
