package archive

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// CompressOption configures Compress.
type CompressOption func(*compressConfig)

type compressConfig struct {
	level int
}

// WithLevel sets the zstd compression level (passed as -<level>, with --ultra
// for levels above 19). When unset or non-positive, the zstd binary's default
// level is used.
func WithLevel(level int) CompressOption {
	return func(c *compressConfig) {
		c.level = level
	}
}

// Compress reads from src and writes a zstd-compressed stream to dst. It shells
// out to the bundled zstd binary — the exact tool whose binary and source ship
// on the recovery disc (SPEC §6) — so the bytes written are produced by the
// same implementation a future recoverer uses to read them.
//
// Compress is the optional second stage of the per-archive prepare pipeline
// (SPEC §4.3); already-compressed sources gain little but are passed through
// without harm.
func Compress(ctx context.Context, dst io.Writer, src io.Reader, opts ...CompressOption) error {
	var cfg compressConfig

	for _, opt := range opts {
		opt(&cfg)
	}

	// -q silences the warning zstd prints when reading from stdin; -c writes the
	// compressed stream to stdout rather than a sibling file.
	args := []string{"-q", "-c"}

	if cfg.level > 0 {
		if cfg.level > 19 {
			args = append(args, "--ultra")
		}

		args = append(args, "-"+strconv.Itoa(cfg.level))
	}

	cmd := exec.CommandContext(ctx, "zstd", args...)
	cmd.Stdin = src
	cmd.Stdout = dst

	var stderr strings.Builder

	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return fmt.Errorf("%s: %w", cmd, err)
	}

	return nil
}
