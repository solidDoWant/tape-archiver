package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Role selects the task queue a worker polls and the activities it registers
// (SPEC §4.1).
type Role string

const (
	// RoleControl runs in Kubernetes: snapshot resolution, report/ISO, Discord.
	RoleControl Role = "control"
	// RoleData runs on the storage host: tar/age/PAR2/checksum/LTFS/changer.
	RoleData Role = "data"
)

// Task queue names (SPEC §4.1).
const (
	controlTaskQueue = "control"
	dataTaskQueue    = "data"
)

func (r Role) taskQueue() string {
	switch r {
	case RoleControl:
		return controlTaskQueue
	case RoleData:
		return dataTaskQueue
	default:
		// Unreachable: parseRole rejects anything else.
		return ""
	}
}

// Config holds the worker-specific startup settings; Temporal and metrics
// settings are read by their own packages.
type Config struct {
	// Role is required.
	Role Role
	// LogLevel is passed to logging.Setup (empty means info).
	LogLevel string
	// IdleExitAfter is the control worker's idle-exit window (#113); zero
	// disables it. The data role ignores it.
	IdleExitAfter time.Duration
}

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

// parseIdleExitAfter parses WORKER_IDLE_EXIT_AFTER; blank means disabled.
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

// parseRole validates ROLE case-insensitively, ignoring surrounding whitespace.
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
