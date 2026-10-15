package ltfs

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// Format formats the loaded tape with mkltfs, naming the volume after its
// barcode (SPEC.md §6).
//
// Safety: Format force-formats, destroying existing contents. Callers must first
// confirm the tape is blank with tape.Drive.IsBlank (SPEC.md §4.3 step 6).
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

// mkltfsArgs builds the mkltfs arguments; --force is safe only because the tape
// is confirmed blank first (see Format).
func mkltfsArgs(device string, volumeName tape.Barcode) []string {
	return []string{
		"--device=" + device,
		"--volume-name=" + string(volumeName),
		"--force",
	}
}
