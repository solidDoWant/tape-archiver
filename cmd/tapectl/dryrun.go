package main

import "github.com/solidDoWant/tape-archiver/internal/config"

// mhvtl device defaults. A dry-run points the worker at the mhvtl virtual tape
// library instead of the real library (SPEC §12). The defaults match the
// devices mhvtl presents in the dev/CI environment; on a host where the real
// library already occupies those nodes, override them via the environment so a
// dry-run never targets real hardware.
const (
	mhvtlChangerEnv = "MHVTL_CHANGER_DEV"
	mhvtlDrive0Env  = "MHVTL_DRIVE0_DEV"
	mhvtlDrive1Env  = "MHVTL_DRIVE1_DEV"

	defaultMHVTLChanger = "/dev/sch0"
	defaultMHVTLDrive0  = "/dev/nst0"
	defaultMHVTLDrive1  = "/dev/nst1"
)

// applyDryRun rewrites the library device targets to the mhvtl virtual library
// so the run exercises virtual hardware instead of the real changer and drives.
// The two mhvtl drives replace whatever drives the config named; the blank
// slots are left untouched, as they are logical positions in the library.
func applyDryRun(cfg *config.Config, getenv func(string) string) {
	cfg.Library.Changer = envOr(getenv, mhvtlChangerEnv, defaultMHVTLChanger)
	cfg.Library.Drives = []string{
		envOr(getenv, mhvtlDrive0Env, defaultMHVTLDrive0),
		envOr(getenv, mhvtlDrive1Env, defaultMHVTLDrive1),
	}
}

// envOr returns the value of the named environment variable, or fallback when
// it is unset or empty.
func envOr(getenv func(string) string, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}

	return fallback
}
