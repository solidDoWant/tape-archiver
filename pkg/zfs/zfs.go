// Package zfs provides read-only access to ZFS snapshots (via .zfs/snapshot/,
// not zfs send) and dataset properties via the zfs CLI (SPEC.md §4.3, §6).
package zfs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SnapshotDir returns the snapshot's directory under
// datasetMount/.zfs/snapshot/, erroring if it does not exist.
func SnapshotDir(datasetMount, snapshot string) (string, error) {
	dir, err := filepath.Abs(filepath.Join(datasetMount, ".zfs", "snapshot", snapshot))
	if err != nil {
		return "", fmt.Errorf("resolve snapshot dir for %q@%q: %w", datasetMount, snapshot, err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("zfs snapshot %q not found at %q: %w", snapshot, dir, err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("zfs snapshot path %q is not a directory", dir)
	}

	return dir, nil
}

// LogicalReferenced returns the logicalreferenced bytes of a dataset or
// snapshot: a cheap size estimate for the feasibility pre-check (SPEC.md §4.3).
func LogicalReferenced(ctx context.Context, dataset string) (int64, error) {
	cmd := exec.CommandContext(ctx, "zfs", "get", "-Hp", "logicalreferenced", dataset)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return 0, fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return 0, fmt.Errorf("%s: %w", cmd, err)
	}

	return parseLogicalReferenced(out)
}

// Mountpoint returns the mountpoint property of a filesystem dataset (not a
// snapshot). Non-path values such as "legacy" or "none" are returned verbatim.
func Mountpoint(ctx context.Context, dataset string) (string, error) {
	cmd := exec.CommandContext(ctx, "zfs", "get", "-Hp", "-o", "value", "mountpoint", dataset)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return "", fmt.Errorf("%s: %w", cmd, err)
	}

	mountpoint := strings.TrimSpace(string(out))
	if mountpoint == "" {
		return "", fmt.Errorf("zfs get mountpoint returned no value for %q", dataset)
	}

	return mountpoint, nil
}

// Mounted reports whether a filesystem dataset is mounted. An unmounted
// dataset's mountpoint can be a stale "shadow" directory, so archiving it would
// silently certify the wrong contents (SPEC.md §6).
func Mounted(ctx context.Context, dataset string) (bool, error) {
	cmd := exec.CommandContext(ctx, "zfs", "get", "-Hp", "-o", "value", "mounted", dataset)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return false, fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return false, fmt.Errorf("%s: %w", cmd, err)
	}

	return parseMounted(out)
}

// parseMounted maps "yes"/"no" to a bool; anything else (e.g. a snapshot's "-")
// is an error rather than a silent false.
func parseMounted(out []byte) (bool, error) {
	switch value := strings.TrimSpace(string(out)); value {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected zfs mounted value %q", value)
	}
}

// UserProperties returns the colon-namespaced user properties of a dataset or
// snapshot. zfs exits non-zero for a missing dataset, so it doubles as an
// existence check (SPEC.md §4.3).
//
// The values are NOT authoritative: a user property value may embed a newline
// that fabricates another record. Security decisions (the democratic-csi
// ownership guard) must use UserProperty instead.
func UserProperties(ctx context.Context, dataset string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "zfs", "get", "-Hp", "-o", "property,value", "all", dataset)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return nil, fmt.Errorf("%s: %w", cmd, err)
	}

	return parseUserProperties(out), nil
}

// UserProperty returns one named user property ("-" when unset). Reading a
// single property cannot be spoofed by newlines in other values, so the
// ownership guard uses it. A missing dataset is an error.
func UserProperty(ctx context.Context, dataset, property string) (string, error) {
	cmd := exec.CommandContext(ctx, "zfs", "get", "-Hp", "-o", "value", property, dataset)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return "", fmt.Errorf("%s: %w", cmd, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// Hold places user hold tag on snapshot so external pruning cannot destroy it
// mid-run (SPEC.md §4.3). Idempotent: "tag already exists" is success.
func Hold(ctx context.Context, tag, snapshot string) error {
	cmd := exec.CommandContext(ctx, "zfs", "hold", tag, snapshot)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "tag already exists") {
			return nil
		}

		if msg != "" {
			return fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return fmt.Errorf("%s: %w", cmd, err)
	}

	return nil
}

// Release removes user hold tag from snapshot. Runs release on every exit path,
// so "no such tag" is treated as success.
func Release(ctx context.Context, tag, snapshot string) error {
	cmd := exec.CommandContext(ctx, "zfs", "release", tag, snapshot)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "no such tag") {
			return nil
		}

		if msg != "" {
			return fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return fmt.Errorf("%s: %w", cmd, err)
	}

	return nil
}

// Holds returns the user-hold tags on snapshot (empty when unheld).
func Holds(ctx context.Context, snapshot string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "zfs", "holds", "-H", snapshot)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return nil, fmt.Errorf("%s: %w", cmd, err)
	}

	return parseHolds(out), nil
}

// parseHolds extracts tags from "zfs holds -H" output (field 2; the timestamp
// may contain spaces but never a tab).
func parseHolds(out []byte) []string {
	var tags []string

	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}

		tags = append(tags, fields[1])
	}

	return tags
}

// parseUserProperties extracts colon-named (user) properties from
// "zfs get -H -o property,value all" output.
func parseUserProperties(out []byte) map[string]string {
	properties := make(map[string]string)

	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}

		name, value, found := strings.Cut(line, "\t")
		if !found || !strings.Contains(name, ":") {
			continue
		}

		properties[name] = value
	}

	return properties
}

// parseLogicalReferenced extracts the value field from "zfs get -Hp" output.
// Dataset names may contain spaces, so split on tabs only: whitespace splitting
// could yield a silently wrong byte count (e.g. "tank/media disc 2").
func parseLogicalReferenced(out []byte) (int64, error) {
	line := strings.TrimSpace(string(out))

	fields := strings.Split(line, "\t")
	if len(fields) < 3 {
		return 0, fmt.Errorf("unexpected zfs get output: %q", line)
	}

	value, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse logicalreferenced %q: %w", fields[2], err)
	}

	return value, nil
}
