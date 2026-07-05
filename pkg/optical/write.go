package optical

import (
	"context"
	"fmt"
)

// WriteImage burns the prepared ISO 9660 image at isoPath onto this disc,
// producing a mountable filesystem. The image is written verbatim via xorriso's
// cdrecord emulation, which performs the MMC track/session finalization a plain
// dd cannot — the reason this is a shell-out and not a block copy (SPEC §10).
//
// WriteImage does not blank the disc first: it assumes the medium is ready to
// write (blank), as the workflow above this seam guarantees before burning
// ("never write to a non-blank tape/disc" — the same invariant as the tape path,
// SPEC §4.3). On a write-once medium that is not blank the burn fails rather than
// corrupting a partially written disc; a rewritable medium must be reclaimed with
// Blank first. It returns a non-nil error if xorriso fails.
func (d *Disc) WriteImage(ctx context.Context, isoPath string) error {
	// -as cdrecord: emulate cdrecord and write the prepared image as-is. -v
	// keeps xorriso's progress on the combined output for diagnostics. No blank=
	// option, so WriteImage is a pure write — it never silently reclaims a disc.
	if _, err := runXorriso(ctx, "-as", "cdrecord", "-v", "dev="+d.driveAddress(), isoPath); err != nil {
		return fmt.Errorf("optical: burning %s to %s: %w", isoPath, d.device, err)
	}

	return nil
}
