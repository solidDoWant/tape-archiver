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

// Per-disc optical burn/verify activities (SPEC §10). They run on the data queue so
// the ISO stays off the Temporal payload path, and are dispatched with
// MaximumAttempts: 1: a failed burn needs an operator decision, not a retry that
// wastes another write-once disc.

// DiscNotWritableErrorType is the non-retryable application-error type BurnDisc
// returns when the loaded disc cannot be written; the workflow pauses for the
// operator on it.
const DiscNotWritableErrorType = "disc-not-writable"

// BurnDiscInput is the payload for the BurnDisc activity.
type BurnDiscInput struct {
	Device string
	// ISOPath is the staged recovery ISO, burned verbatim (no computation in the
	// burn window).
	ISOPath string
	// AllowNonBlankDiscs reclaims a non-blank rewritable disc instead of pausing. It
	// never forces a write-once overwrite.
	AllowNonBlankDiscs bool
	// DriveHasVerifiedCopy is true when this run already burned and verified a copy
	// in this burner. A non-blank disc there is then this run's own copy, so it is
	// never reclaimed even with AllowNonBlankDiscs (issue #154).
	DriveHasVerifiedCopy bool
}

// BurnResult is the BurnDisc activity's result, recorded in the report (SPEC §9).
type BurnResult struct {
	Device string
	// OverwroteNonBlank is true when a non-blank rewritable disc was reclaimed.
	OverwroteNonBlank bool
}

// VerifyDiscInput is the payload for the VerifyDisc activity.
type VerifyDiscInput struct {
	Device string
	// ManifestPath is a sha256sum-format manifest of the disc's contents.
	ManifestPath string
}

// BurnActivities hosts the stateless BurnDisc and VerifyDisc activities.
type BurnActivities struct{}

// newBurnActivities returns the optical burn/verify activities.
func newBurnActivities() *BurnActivities { return &BurnActivities{} }

// burnAction is the decision the overwrite policy reaches for a loaded disc.
type burnAction int

const (
	burnWrite burnAction = iota
	// burnReclaimWrite blanks a non-blank rewritable disc, then burns.
	burnReclaimWrite
	// burnPause returns the operator-pause error without touching the disc.
	burnPause
)

// decideBurn applies the overwrite policy to a loaded disc's state. It is pure so
// the "never silently overwrite" decision is unit-testable without hardware.
func decideBurn(state optical.DiscState, allowNonBlank, driveHasVerifiedCopy bool) (burnAction, error) {
	switch state {
	case optical.StateBlank:
		return burnWrite, nil
	case optical.StateNonBlankRewritable:
		// A non-blank disc in a drive that already produced a verified copy is this
		// run's own copy; reclaiming it would destroy a counted copy (issue #154).
		if allowNonBlank && !driveHasVerifiedCopy {
			return burnReclaimWrite, nil
		}

		return burnPause, nil
	case optical.StateAppendableWriteOnce, optical.StateFinalized:
		// Write-once media can never be reclaimed, opt-in or not (SPEC §10).
		return burnPause, nil
	default: // StateUnknown
		return burnWrite, fmt.Errorf("optical disc state could not be determined (no medium loaded, or an unreadable disc)")
	}
}

// BurnDisc burns the staged recovery ISO onto the loaded disc after applying the
// overwrite policy; an unwritable disc yields IsDiscNotWritable and nothing is written.
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

	action, err := decideBurn(state, input.AllowNonBlankDiscs, input.DriveHasVerifiedCopy)
	if err != nil {
		return BurnResult{}, fmt.Errorf("optical burn to %s: %w", input.Device, err)
	}

	if action == burnPause {
		return BurnResult{}, discNotWritableError(input.Device, state, input.AllowNonBlankDiscs, input.DriveHasVerifiedCopy)
	}

	slog.InfoContext(ctx, "optical: burning recovery ISO to disc",
		"device", input.Device, "iso", input.ISOPath, "discState", state.String())

	result := BurnResult{Device: input.Device}

	err = withActivityHeartbeat(ctx, func() error {
		if action == burnReclaimWrite {
			// Irreversible: warn so the destroyed data is visible in the run log.
			slog.WarnContext(ctx, "optical: reclaiming a NON-BLANK rewritable disc before burning "+
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

	slog.InfoContext(ctx, "optical: burned recovery disc",
		"device", input.Device, "overwroteNonBlank", result.OverwroteNonBlank)

	return result, nil
}

// discNotWritableError builds the operator-pause error naming the drive and reason.
func discNotWritableError(device string, state optical.DiscState, allowNonBlank, driveHasVerifiedCopy bool) error {
	var reason string

	switch {
	case state == optical.StateNonBlankRewritable && driveHasVerifiedCopy:
		reason = "a copy this run already burned and verified is still loaded in this drive;" +
			" load a fresh blank disc so this copy is a distinct disc, not an overwrite of the last one"
	case state == optical.StateNonBlankRewritable:
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

// IsDiscNotWritable reports whether err is BurnDisc's operator-pause error.
func IsDiscNotWritable(err error) bool {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return false
	}

	return appErr.Type() == DiscNotWritableErrorType
}

// VerifyDisc reads the burned disc back and checks every file against the manifest;
// any mismatch, missing, or extra file fails the disc.
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

	if err := withActivityHeartbeat(ctx, func() error {
		result, err := disc.Verify(ctx, manifest)
		if err != nil {
			return fmt.Errorf("read back disc in %s: %w", input.Device, err)
		}

		return result.Err()
	}); err != nil {
		return err
	}

	slog.InfoContext(ctx, "optical: verified burned disc; every file matches the disc manifest",
		"device", input.Device, "files", len(manifest))

	return nil
}

// readManifestFile parses the sha256sum-format disc manifest at path.
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
