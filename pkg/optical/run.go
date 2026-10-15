package optical

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runXorriso runs xorriso and returns its combined output, also on error. The
// streams are combined because xorriso writes its media report to stderr.
func runXorriso(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, xorrisoBin, args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return string(out), fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return string(out), fmt.Errorf("%s: %w", cmd, err)
	}

	return string(out), nil
}
