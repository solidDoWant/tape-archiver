package ltfs

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// Format formats the loaded tape with mkltfs, setting the LTFS volume name to
// the tape's barcode (SPEC.md §6: the library barcode is the canonical physical
// ID and the LTFS volume name is set to it). It returns a non-nil error if
// mkltfs fails.
//
// Safety: Format unconditionally formats the tape (mkltfs -f), destroying any
// existing contents. The guarantee that a run never silently overwrites data
// lives upstream — the Load phase confirms the tape is blank with
// tape.Drive.IsBlank before the Write phase calls Format (SPEC.md §4.3 step 6,
// "Never write to a non-blank tape"). Callers must honor that ordering.
func (v *Volume) Format(ctx context.Context, volumeName tape.Barcode) error {
	cmd := exec.CommandContext(ctx, "mkltfs", mkltfsArgs(v.device, volumeName)...)

	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return fmt.Errorf("%s: %w", cmd, err)
	}

	return nil
}

// mkltfsArgs builds the mkltfs argument list: format the device, set the volume
// name to the barcode, and force the format (-f) since the tape is confirmed
// blank before this runs (see Format).
func mkltfsArgs(device string, volumeName tape.Barcode) []string {
	return []string{
		"--device=" + device,
		"--volume-name=" + string(volumeName),
		"--force",
	}
}
