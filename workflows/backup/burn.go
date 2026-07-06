package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"go.temporal.io/sdk/temporal"

	"github.com/solidDoWant/tape-archiver/pkg/optical"
)

// The optical-burn phase (SPEC §10) burns the recovery disc — the PDF report, the
// SHA-256 manifest, the per-tape LTFS indexes, and the static recovery binaries —
// onto optical media (M-DISC DVD) as an extra, offline redundancy layer. These are
// the per-disc activities that physically burn and verify ONE disc; they run on the
// data task queue (the storage host, where the staged ISO lives and the burners are
// attached), keeping the tens-of-MB image off the Temporal payload path exactly like
// the tape Write/Eject activities.
//
// The deterministic burn-set / disc-swap / pause-resume orchestration is a separate
// sub-issue of parent #98: BurnDisc signals "cannot write, pause the operator" via a
// distinguishable error (IsDiscNotWritable) rather than blocking, and never silently
// overwrites a disc.
//
// Both activities carry MaximumAttempts: 1 in their workflow dispatch — a burn is
// physical and its failure (a bad disc, a refused non-blank medium) needs an operator
// decision, not a blind retry that would waste another write-once disc.

// DiscNotWritableErrorType is the temporal application-error type BurnDisc returns
// when the loaded disc cannot be written — a non-blank disc without the allow-non-blank
// opt-in, or any non-blank write-once disc (which the opt-in never forces). The
// orchestration sub-issue matches on it (IsDiscNotWritable) to pause for the operator
// instead of failing the run. It is non-retryable: the disc is physically unwritable
// until the operator swaps it, so retrying the same attempt cannot succeed.
//
// The BurnDisc/VerifyDisc activity timeouts (StartToClose, HeartbeatTimeout) are set by
// the workflow dispatch in the burn-set orchestration sub-issue of parent #98.
const DiscNotWritableErrorType = "disc-not-writable"

// BurnDiscInput is the payload for the BurnDisc activity: the burner device, the
// staged uncompressed ISO to burn, and whether the run opted in to reclaiming a
// non-blank rewritable disc (Delivery.OpticalBurn.AllowNonBlankDiscs).
type BurnDiscInput struct {
	// Device is the optical burner device node (e.g. /dev/sr0).
	Device string
	// ISOPath is the staged, uncompressed recovery ISO 9660 image to burn. It is the
	// image assembled by the Report phase (SPEC §10) — burned verbatim, no computation
	// in the burn window.
	ISOPath string
	// AllowNonBlankDiscs mirrors Delivery.OpticalBurn.AllowNonBlankDiscs: when true a
	// non-blank *rewritable* disc is reclaimed and overwritten (with a warning)
	// instead of pausing the run. It never forces a write-once overwrite. The state
	// detection itself always runs; this only changes the non-blank outcome.
	AllowNonBlankDiscs bool
}

// BurnResult is the BurnDisc activity's result, carrying the provenance the report
// records (SPEC §9): which drive burned the disc and whether a non-blank disc was
// deliberately reclaimed.
type BurnResult struct {
	// Device is the burner the disc was written on, echoed for the run record.
	Device string
	// OverwroteNonBlank is true when the disc was found non-blank (rewritable) and
	// reclaimed because the run set AllowNonBlankDiscs. It is false for the normal
	// (blank) path. The report records the deliberate overwrite.
	OverwroteNonBlank bool
}

// VerifyDiscInput is the payload for the VerifyDisc activity.
type VerifyDiscInput struct {
	// Device is the optical burner/reader device node the burned disc is loaded in.
	Device string
	// ManifestPath is the on-disk path to the sha256sum-format manifest of the disc's
	// contents (disc-relative path -> SHA-256). VerifyDisc reads the disc back and
	// checks every file against it. The disc-content manifest is produced by the
	// Report/orchestration sub-issue; VerifyDisc is agnostic to how it was built.
	ManifestPath string
}

// BurnActivities hosts the per-disc BurnDisc and VerifyDisc activities. It is
// stateless (like EjectActivities): every per-disc parameter flows through the
// activity input, so nothing is wired in from DataConfig.
type BurnActivities struct{}

// newBurnActivities returns the optical burn/verify activities.
func newBurnActivities() *BurnActivities { return &BurnActivities{} }

// burnAction is the decision the overwrite policy reaches for a loaded disc.
type burnAction int

const (
	// burnWrite: the disc is blank and ready to burn directly.
	burnWrite burnAction = iota
	// burnReclaimWrite: the disc is a non-blank rewritable medium and the run opted
	// in — reclaim it with Blank, then burn.
	burnReclaimWrite
	// burnPause: the disc cannot be written (non-blank without the opt-in, or any
	// non-blank write-once medium) — return the operator-pause error, touch nothing.
	burnPause
)

// decideBurn applies the overwrite policy to a loaded disc's state. It is pure —
// no device I/O — so the "never silently overwrite" decision that ACs 3–5 turn on
// is unit-tested exhaustively without optical hardware (mirroring optical.verifyTree).
//
//   - StateBlank                                  → burnWrite
//   - StateNonBlankRewritable + allowNonBlank      → burnReclaimWrite
//   - StateNonBlankRewritable + !allowNonBlank     → burnPause
//   - StateAppendableWriteOnce / StateFinalized    → burnPause (write-once: the flag
//     never forces an overwrite, so a used write-once disc always pauses)
//   - StateUnknown                                 → error (no/unreadable medium)
func decideBurn(state optical.DiscState, allowNonBlank bool) (burnAction, error) {
	switch state {
	case optical.StateBlank:
		return burnWrite, nil
	case optical.StateNonBlankRewritable:
		if allowNonBlank {
			return burnReclaimWrite, nil
		}

		return burnPause, nil
	case optical.StateAppendableWriteOnce, optical.StateFinalized:
		// Write-once media (M-DISC DVD-R, DVD+R, CD-R, BD-R) cannot be reclaimed;
		// AllowNonBlankDiscs deliberately does not apply here (SPEC §10, AC5).
		return burnPause, nil
	default: // StateUnknown
		return burnWrite, fmt.Errorf("optical disc state could not be determined (no medium loaded, or an unreadable disc)")
	}
}

// BurnDisc burns the staged recovery ISO onto the disc loaded in input.Device,
// applying the overwrite policy first: a blank disc is written directly; a non-blank
// rewritable disc is reclaimed and written only when the run set AllowNonBlankDiscs
// (with a prominent warning naming the drive); any other non-blank disc — a rewritable
// one without the opt-in, or a write-once one regardless of the opt-in — is refused
// with a typed operator-pause error (IsDiscNotWritable) and nothing is written, so no
// partial or bad disc is produced.
//
// The long burn (and any reclaim) runs under a liveness heartbeat so a data-worker
// restart mid-burn is caught within activityHeartbeatTimeout rather than the activity's
// (much longer) StartToClose timeout.
func (a *BurnActivities) BurnDisc(ctx context.Context, input BurnDiscInput) (BurnResult, error) {
	if input.Device == "" {
		return BurnResult{}, fmt.Errorf("no optical burner device configured")
	}

	if input.ISOPath == "" {
		return BurnResult{}, fmt.Errorf("no recovery ISO path provided to burn to %s", input.Device)
	}

	disc := optical.NewDisc(input.Device)

	state, err := disc.State(ctx)
	if err != nil {
		return BurnResult{}, fmt.Errorf("read state of disc in %s: %w", input.Device, err)
	}

	action, err := decideBurn(state, input.AllowNonBlankDiscs)
	if err != nil {
		return BurnResult{}, fmt.Errorf("optical burn to %s: %w", input.Device, err)
	}

	if action == burnPause {
		return BurnResult{}, discNotWritableError(input.Device, state, input.AllowNonBlankDiscs)
	}

	result := BurnResult{Device: input.Device}

	err = withActivityHeartbeat(ctx, func() error {
		if action == burnReclaimWrite {
			// Reclaiming a non-blank disc is deliberate (AllowNonBlankDiscs) but
			// irreversible: warn loudly, naming the drive whose disc is being wiped,
			// so the destroyed data is observable in the run's durable log (SPEC §10).
			slog.Warn("optical: reclaiming a NON-BLANK rewritable disc before burning "+
				"(Delivery.OpticalBurn.AllowNonBlankDiscs is set); existing disc contents will be destroyed",
				"device", input.Device)

			if err := disc.Blank(ctx); err != nil {
				return fmt.Errorf("reclaim non-blank disc in %s: %w", input.Device, err)
			}

			result.OverwroteNonBlank = true
		}

		if err := disc.WriteImage(ctx, input.ISOPath); err != nil {
			return fmt.Errorf("burn %s to %s: %w", input.ISOPath, input.Device, err)
		}

		return nil
	})
	if err != nil {
		return BurnResult{}, err
	}

	return result, nil
}

// discNotWritableError builds the typed, non-retryable operator-pause error for a
// disc that cannot be written, naming the drive and the reason (a rewritable disc that
// needs the opt-in, or a write-once disc the opt-in can never overwrite).
func discNotWritableError(device string, state optical.DiscState, allowNonBlank bool) error {
	var reason string

	switch state {
	case optical.StateNonBlankRewritable:
		reason = fmt.Sprintf("disc is non-blank (%s) and Delivery.OpticalBurn.AllowNonBlankDiscs is not set;"+
			" load a blank disc to continue", state)
	default:
		reason = fmt.Sprintf("disc is non-blank write-once (%s) and cannot be overwritten", state)
		if allowNonBlank {
			reason += " even with Delivery.OpticalBurn.AllowNonBlankDiscs set"
		}

		reason += "; load a blank disc to continue"
	}

	return temporal.NewNonRetryableApplicationError(
		fmt.Sprintf("optical burn to %s: %s", device, reason),
		DiscNotWritableErrorType,
		nil,
	)
}

// IsDiscNotWritable reports whether err is the typed operator-pause error BurnDisc
// returns when a disc cannot be written. The orchestration sub-issue uses it to pause
// for the operator (swap in a blank disc) rather than failing the run.
func IsDiscNotWritable(err error) bool {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return false
	}

	return appErr.Type() == DiscNotWritableErrorType
}

// VerifyDisc mounts the burned disc in input.Device read-only and confirms every file
// matches the disc-content manifest at input.ManifestPath (disc-relative path ->
// SHA-256), returning an error on any mismatch, missing, or extra file — a burn
// failure. The read-back runs under a liveness heartbeat. A returned error is either
// operational (mount/read/parse) or a content mismatch (VerifyResult.Err); both fail
// the disc, which the orchestration sub-issue treats as a bad burn.
func (a *BurnActivities) VerifyDisc(ctx context.Context, input VerifyDiscInput) error {
	if input.Device == "" {
		return fmt.Errorf("no optical device configured to verify")
	}

	if input.ManifestPath == "" {
		return fmt.Errorf("no manifest path provided to verify disc in %s against", input.Device)
	}

	manifest, err := readManifestFile(input.ManifestPath)
	if err != nil {
		return err
	}

	disc := optical.NewDisc(input.Device)

	return withActivityHeartbeat(ctx, func() error {
		result, err := disc.Verify(ctx, manifest)
		if err != nil {
			return fmt.Errorf("read back disc in %s: %w", input.Device, err)
		}

		return result.Err()
	})
}

// readManifestFile reads and parses the sha256sum-format disc-content manifest at
// path into an optical.Manifest.
func readManifestFile(path string) (optical.Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open disc manifest %s: %w", path, err)
	}

	defer func() { _ = file.Close() }()

	manifest, err := optical.ParseManifest(file)
	if err != nil {
		return nil, fmt.Errorf("parse disc manifest %s: %w", path, err)
	}

	return manifest, nil
}
