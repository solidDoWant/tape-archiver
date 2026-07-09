package main

import (
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
func applyDryRun(cfg *config.Config, getenv func(string) string) error {
	return runsubmit.ApplyDryRun(cfg, getenv, warnOut)
}
