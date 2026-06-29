package main

import (
	"fmt"
	"os"
	"strings"
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
}

// parseConfig reads the worker configuration from the environment.
func parseConfig() (Config, error) {
	role, err := parseRole(os.Getenv("ROLE"))
	if err != nil {
		return Config{}, err
	}

	return Config{
		Role:     role,
		LogLevel: os.Getenv("LOG_LEVEL"),
	}, nil
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
