package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/runsubmit"
)

// warnOut receives dry-run advisories on stderr, since stdout carries the
// workflow ID callers may parse.
var warnOut io.Writer = os.Stderr

// mhvtl device environment variable names, aliased from pkg/runsubmit.
const (
	mhvtlChangerEnv = runsubmit.MHVTLChangerEnv
	mhvtlDrive0Env  = runsubmit.MHVTLDrive0Env
	mhvtlDrive1Env  = runsubmit.MHVTLDrive1Env
)

// applyDryRun wraps pkg/runsubmit.ApplyDryRun, naming the --dry-run flag in the
// missing-env error.
func applyDryRun(cfg *config.Config, getenv func(string) string) error {
	err := runsubmit.ApplyDryRun(cfg, getenv, warnOut)

	var missingEnv *runsubmit.MissingMHVTLEnvError
	if errors.As(err, &missingEnv) {
		return fmt.Errorf("--%w", missingEnv)
	}

	return err
}
