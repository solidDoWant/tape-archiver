package optical

import (
	"context"
	"fmt"
)

// Blank reclaims a rewritable disc so a subsequent WriteImage can rewrite it,
// using xorriso's `-blank as_needed` (a no-op on an already-blank medium). It
// first probes the medium and refuses a write-once one: a write-once disc (an
// M-DISC DVD-R, DVD+R, CD-R, BD-R) cannot be reclaimed, so Blank returns an error
// rather than silently succeeding and leaving the caller believing the disc is
// reusable. A finalized medium is likewise refused.
//
// Blank is a physical reclaim (it discards the disc's contents). The decision of
// whether reclaiming is permitted for this run lives above this seam; Blank only
// performs the reclaim it is asked for, and only when the medium physically
// supports it.
func (d *Disc) Blank(ctx context.Context) error {
	info, err := d.probe(ctx)
	if err != nil {
		return err
	}

	if err := blankable(info); err != nil {
		return fmt.Errorf("optical: cannot blank %s: %w", d.device, err)
	}

	if _, err := runXorriso(ctx, "-outdev", d.driveAddress(), "-blank", "as_needed"); err != nil {
		return fmt.Errorf("optical: blanking %s: %w", d.device, err)
	}

	return nil
}

// blankable reports whether the probed medium may be reclaimed with `-blank`,
// returning a descriptive error when it may not. It is pure (no device I/O) so the
// refusal Blank documents is unit-testable without real media. Two media are
// refused, matching Blank's contract:
//
//   - write-once media (M-DISC DVD-R, DVD+R, CD-R, BD-R): they cannot be reclaimed
//     at all, so blanking one would fail or silently no-op and leave the caller
//     believing the disc is reusable;
//   - finalized media (a closed disc, StateFinalized): closed to any further write,
//     including a rewritable disc closed after burning — `parseMediaReport`
//     classifies a closed DVD-RW as StateFinalized with rewritable=true, so the
//     rewritability check alone would wrongly admit it.
func blankable(info mediaInfo) error {
	if !info.rewritable {
		return fmt.Errorf("medium is write-once (%s), not rewritable", info.state)
	}

	if info.state == StateFinalized {
		return fmt.Errorf("medium is finalized (%s) and can no longer be written or reclaimed", info.state)
	}

	return nil
}
