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

// WithLevel sets the zstd level; non-positive uses zstd's default.
func WithLevel(level int) CompressOption {
	return func(c *compressConfig) {
		c.level = level
	}
}

// Compress zstd-compresses src to dst. It shells out to the bundled zstd so the
// bytes come from the same tool shipped on the recovery disc (SPEC §6).
func Compress(ctx context.Context, dst io.Writer, src io.Reader, opts ...CompressOption) error {
	var cfg compressConfig

	for _, opt := range opts {
		opt(&cfg)
	}

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
