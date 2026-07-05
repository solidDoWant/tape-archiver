package optical

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runXorriso runs xorriso with args, returning its combined stdout+stderr. On a
// non-zero exit it returns the output alongside an error wrapping the command and
// the trimmed output (matching pkg/ltfs's error style). The output is returned
// even on success so callers that parse xorriso's report (State) can read it; it
// is also returned on error so the caller can inspect a partial report.
//
// xorriso writes its progress and media report to stderr and exits non-zero only
// on a real failure, so combining the streams captures the full report either
// way.
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
