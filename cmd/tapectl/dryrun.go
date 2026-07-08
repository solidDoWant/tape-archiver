package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

// warnOut receives dry-run advisories. It is stderr — never stdout, which carries
// the submitted workflow ID that callers may parse — and is a package-level var so
// tests can capture the advisory (mirrors the getenv indirection).
var warnOut io.Writer = os.Stderr

// mhvtl device environment variables. A dry-run points the worker at the mhvtl
// virtual tape library instead of the real library (SPEC §12). There is no
// hardware default: the fallback nodes (`/dev/sch0`, `/dev/nstX`) are
// byte-identical to the real library, so a missing override would silently
// aim a dry-run at real hardware. Instead every variable is required, and a
// dry-run with any of them unset fails fast (see applyDryRun).
const (
	mhvtlChangerEnv = "MHVTL_CHANGER_DEV"
	mhvtlDrive0Env  = "MHVTL_DRIVE0_DEV"
	mhvtlDrive1Env  = "MHVTL_DRIVE1_DEV"
)

// applyDryRun rewrites the library device targets to the mhvtl virtual library
// so the run exercises virtual hardware instead of the real changer and drives.
// The two mhvtl drives replace whatever drives the config named; the blank
// slots are left untouched, as they are logical positions in the library.
//
// The mhvtl device nodes must be named explicitly via the environment. Because
// the fallback nodes would be indistinguishable from the real library — and the
// devices are opened worker-side while these variables are read client-side, so
// the submitted config carries no dry-run marker the worker could honor — a
// dry-run with any variable unset returns an actionable error and rewrites
// nothing, rather than silently targeting real hardware (CLAUDE.md Hardware and
// Safety; SPEC §12).
//
// applyDryRun also disables optical burning. mhvtl provides no virtual optical
// burner, so — unlike the tape library — there is no safe device to redirect to,
// and the submitted config carries no dry-run marker the worker could honor. Left
// in place, delivery.opticalBurn keeps OpticalBurn.Enabled() true and the worker
// would probe, pause on, blank, and irreversibly burn the operator's real burner
// during what is meant to be a hardware-free test. Neutralizing the section
// (rather than refusing the whole config) keeps the tape path — which mhvtl can
// exercise end to end — dry-runnable for configs that also configure burning; the
// run then completes exactly as a no-optical-burn run (burnPhase is a no-op). An
// advisory is emitted so the operator knows burning was skipped for the dry-run.
func applyDryRun(cfg *config.Config, getenv func(string) string) error {
	changer := getenv(mhvtlChangerEnv)
	drive0 := getenv(mhvtlDrive0Env)
	drive1 := getenv(mhvtlDrive1Env)

	var missing []string

	if changer == "" {
		missing = append(missing, mhvtlChangerEnv)
	}

	if drive0 == "" {
		missing = append(missing, mhvtlDrive0Env)
	}

	if drive1 == "" {
		missing = append(missing, mhvtlDrive1Env)
	}

	if len(missing) != 0 {
		return fmt.Errorf("--dry-run requires the mhvtl virtual-library device(s) to be named "+
			"explicitly, but %s %s unset; set them to the mhvtl nodes (the dev shell's `mhvtl-up` "+
			"exports these) so a dry-run never targets real hardware",
			strings.Join(missing, ", "), pluralIsAre(len(missing)))
	}

	cfg.Library.Changer = changer
	cfg.Library.Drives = []string{drive0, drive1}

	// Disable optical burning: there is no virtual burner to redirect to, so the
	// only safe target is off. Enabled() is nil-safe, so this is a no-op when the
	// section is absent or already disabled.
	if cfg.Delivery.OpticalBurn.Enabled() {
		// Best-effort advisory: a failed write to stderr must not fail the dry-run.
		_, _ = fmt.Fprintln(warnOut, "tapectl: dry-run: optical burning disabled — mhvtl provides no "+
			"virtual optical burner, so delivery.opticalBurn is skipped and the real burner is "+
			"never probed, blanked, or written")
	}

	cfg.Delivery.OpticalBurn = nil

	return nil
}

// pluralIsAre returns the correct verb form for the count of missing variables.
func pluralIsAre(count int) string {
	if count == 1 {
		return "is"
	}

	return "are"
}
