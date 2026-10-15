package optical

import (
	"context"
	"fmt"
)

// Blank reclaims a rewritable disc (discarding its contents), refusing write-once
// and finalized media rather than appearing to succeed. Whether reclaiming is
// permitted is decided by the caller.
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

// blankable refuses write-once and finalized media. The finalized check is
// separate because a closed DVD-RW is StateFinalized with rewritable=true.
func blankable(info mediaInfo) error {
	if !info.rewritable {
		return fmt.Errorf("medium is write-once (%s), not rewritable", info.state)
	}

	if info.state == StateFinalized {
		return fmt.Errorf("medium is finalized (%s) and can no longer be written or reclaimed", info.state)
	}

	return nil
}
