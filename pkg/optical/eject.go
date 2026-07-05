package optical

import (
	"context"
	"fmt"
)

// Eject ejects the disc from the drive. On a real optical drive this opens the
// tray; on the stdio pseudo-disc used in tests it is a harmless no-op, so the
// same code runs against file-backed media. It returns a non-nil error if
// xorriso fails.
func (d *Disc) Eject(ctx context.Context) error {
	if _, err := runXorriso(ctx, "-outdev", d.driveAddress(), "-eject", "all"); err != nil {
		return fmt.Errorf("optical: ejecting %s: %w", d.device, err)
	}

	return nil
}
