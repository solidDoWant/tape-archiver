package optical

import (
	"context"
	"fmt"
)

// Eject ejects the disc; on the stdio pseudo-disc it is a no-op.
func (d *Disc) Eject(ctx context.Context) error {
	if _, err := runXorriso(ctx, "-outdev", d.driveAddress(), "-eject", "all"); err != nil {
		return fmt.Errorf("optical: ejecting %s: %w", d.device, err)
	}

	return nil
}
