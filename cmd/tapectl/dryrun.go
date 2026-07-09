package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/solidDoWant/tape-archiver/internal/config"
	"github.com/solidDoWant/tape-archiver/pkg/runsubmit"
)

// warnOut receives dry-run advisories. It is stderr — never stdout, which carries
// the submitted workflow ID that callers may parse — and is a package-level var so
// tests can capture the advisory (mirrors the getenv indirection).
var warnOut io.Writer = os.Stderr

// mhvtl device environment variable names, aliased from pkg/runsubmit (the
// single source of truth, also used by pkg/runsapi's POST /api/runs dry-run
// path) so this package's flag/error text and tests can keep using the
// short, lower-case local names.
const (
	mhvtlChangerEnv = runsubmit.MHVTLChangerEnv
	mhvtlDrive0Env  = runsubmit.MHVTLDrive0Env
	mhvtlDrive1Env  = runsubmit.MHVTLDrive1Env
)

// applyDryRun rewrites cfg's library device targets to the mhvtl virtual
// library and disables optical burning — see
// pkg/runsubmit.ApplyDryRun's doc comment for the full rationale (fail-closed
// on missing env, no safe optical-burn redirect, re-validation). It is a
// thin wrapper so both `tapectl run --dry-run` and pkg/runsapi's `POST
// /api/runs` dry-run flag share one implementation and can never drift.
//
// The one place this wrapper does more than delegate: pkg/runsubmit's
// missing-env message is deliberately caller-agnostic (it doesn't name a
// flag, since pkg/runsapi's caller has no `--dry-run` flag to name); this CLI
// names its own flag explicitly, as it always has, by prefixing that specific
// error.
func applyDryRun(cfg *config.Config, getenv func(string) string) error {
	err := runsubmit.ApplyDryRun(cfg, getenv, warnOut)

	var missingEnv *runsubmit.MissingMHVTLEnvError
	if errors.As(err, &missingEnv) {
		return fmt.Errorf("--%w", missingEnv)
	}

	return err
}
