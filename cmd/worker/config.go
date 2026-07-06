package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Role selects which task queue a worker polls and which set of activities it
// registers. The two roles map one-to-one onto the two task queues described in
// SPEC §4.1.
type Role string

const (
	// RoleControl runs in Kubernetes and handles the lightweight, k8s-side
	// activities: VolumeSnapshot resolution, PDF report building, recovery ISO
	// building, and Discord delivery.
	RoleControl Role = "control"
	// RoleData runs on the storage host and handles the bulk-data activities:
	// tar/compress/split, age encryption, PAR2 generation, checksum
	// verification, LTFS format/mount/write/unmount, and changer load/unload.
	RoleData Role = "data"
)

// Task queue names (SPEC §4.1). They share their spelling with the role values,
// but are kept as distinct constants so the queue topology is explicit and can
// diverge from the role names without code changes elsewhere.
const (
	controlTaskQueue = "control"
	dataTaskQueue    = "data"
)

// taskQueue returns the Temporal task queue the role polls.
func (r Role) taskQueue() string {
	switch r {
	case RoleControl:
		return controlTaskQueue
	case RoleData:
		return dataTaskQueue
	default:
		// Unreachable: Config values originate from parseRole, which rejects
		// anything other than the two roles above.
		return ""
	}
}

// Config holds the worker's startup configuration parsed from the environment.
// Temporal connection settings (TEMPORAL_ADDRESS, TEMPORAL_NAMESPACE, and the
// rest) are consumed by pkg/temporalclient via the Temporal envconfig loader,
// and the metrics listen address by pkg/metrics; this struct owns only the
// settings unique to the worker binary.
type Config struct {
	// Role selects the task queue and activity set. Required.
	Role Role
	// LogLevel is passed verbatim to logging.Setup (empty defaults to info).
	LogLevel string
	// IdleExitAfter is the control worker's idle window: once no activity task
	// has run on this worker for this duration, it drains and exits 0 so a
	// KEDA-spawned Job can scale back to zero (SPEC §4.1; parent #113). Zero
	// disables idle-exit (the default), leaving the worker running until
	// SIGINT/SIGTERM. It is honored only for the control role; the data worker
	// (systemd on the storage host, no KEDA) ignores it.
	IdleExitAfter time.Duration
}

// parseConfig reads the worker configuration from the environment.
func parseConfig() (Config, error) {
	role, err := parseRole(os.Getenv("ROLE"))
	if err != nil {
		return Config{}, err
	}

	idleExitAfter, err := parseIdleExitAfter(os.Getenv("WORKER_IDLE_EXIT_AFTER"))
	if err != nil {
		return Config{}, err
	}

	return Config{
		Role:          role,
		LogLevel:      os.Getenv("LOG_LEVEL"),
		IdleExitAfter: idleExitAfter,
	}, nil
}

// parseIdleExitAfter parses the WORKER_IDLE_EXIT_AFTER idle window. An empty or
// whitespace-only value yields a zero duration, which disables idle-exit. A
// non-empty value must be a valid Go duration string (e.g. "15m"); a negative
// duration is rejected since it has no meaningful idle-window interpretation.
func parseIdleExitAfter(raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}

	idleExitAfter, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("WORKER_IDLE_EXIT_AFTER must be a valid duration (got %q): %w", raw, err)
	}

	if idleExitAfter < 0 {
		return 0, fmt.Errorf("WORKER_IDLE_EXIT_AFTER must not be negative (got %q)", raw)
	}

	return idleExitAfter, nil
}

// parseRole validates the ROLE environment variable. Matching is
// case-insensitive and ignores surrounding whitespace. An empty or
// unrecognized value is an error naming the accepted roles.
func parseRole(raw string) (Role, error) {
	switch Role(strings.ToLower(strings.TrimSpace(raw))) {
	case RoleControl:
		return RoleControl, nil
	case RoleData:
		return RoleData, nil
	default:
		return "", fmt.Errorf("ROLE must be %q or %q (got %q)", RoleControl, RoleData, raw)
	}
}
