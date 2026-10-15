package optical

import (
	"context"
	"fmt"
)

// WriteImage burns the ISO 9660 image at isoPath onto this disc via cdrecord
// emulation, which performs the MMC finalization dd cannot (SPEC §10). It never
// blanks: the caller guarantees a blank medium ("never write to a non-blank
// disc", SPEC §4.3), and a non-blank write-once disc makes the burn fail.
func (d *Disc) WriteImage(ctx context.Context, isoPath string) error {
	// No blank= option, so this can never silently reclaim a disc.
	if _, err := runXorriso(ctx, "-as", "cdrecord", "-v", "dev="+d.driveAddress(), isoPath); err != nil {
		return fmt.Errorf("optical: burning %s to %s: %w", isoPath, d.device, err)
	}

	return nil
}
