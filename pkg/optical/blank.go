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

	if !info.rewritable {
		return fmt.Errorf("optical: cannot blank %s: medium is write-once (%s), not rewritable", d.device, info.state)
	}

	if _, err := runXorriso(ctx, "-outdev", d.driveAddress(), "-blank", "as_needed"); err != nil {
		return fmt.Errorf("optical: blanking %s: %w", d.device, err)
	}

	return nil
}
