package tape

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Drive wraps a tape drive device node for drive-level operations.
// Use the non-rewinding device node (e.g. /dev/nst0).
type Drive struct {
	device string
}

// NewDrive returns a Drive targeting the given non-rewinding tape device
// (e.g. /dev/nst0).
func NewDrive(device string) *Drive {
	return &Drive{device: device}
}

// IsBlank reports whether the loaded tape is blank (never written or fully erased).
//
// It rewinds to BOT then attempts to read one block. A read returning zero bytes
// (immediate EOD) means blank; any data means the tape has content and must not
// be overwritten (SPEC.md §4.3 step 6).
func (d *Drive) IsBlank(ctx context.Context) (blank bool, err error) {
	if err = d.rewind(ctx); err != nil {
		return false, err
	}

	f, err := os.Open(d.device)
	if err != nil {
		return false, fmt.Errorf("open tape device %s: %w", d.device, err)
	}

	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close tape device %s: %w", d.device, cerr)
		}
	}()

	// 64 KiB is large enough for any fixed-block-size tape drive to return one block.
	buf := make([]byte, 64*1024)

	n, readErr := f.Read(buf)
	if n > 0 {
		// Data found — tape is not blank.
		return false, nil
	}

	if readErr == io.EOF || readErr == nil {
		// EOD immediately at BOT — tape is blank.
		return true, nil
	}

	return false, fmt.Errorf("read tape device %s: %w", d.device, readErr)
}

// rewind runs "mt -f <device> rewind" to position the tape at BOT.
func (d *Drive) rewind(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "mt", "-f", d.device, "rewind")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mt -f %s rewind: %w: %s", d.device, err, out)
	}

	return nil
}
