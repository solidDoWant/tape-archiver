// Package zfs provides read-only access to ZFS snapshots and dataset
// properties needed by the backup pipeline. Snapshot contents are reached
// through the dataset's .zfs/snapshot/ directory (no zfs send), and dataset
// properties are read by shelling out to the zfs CLI (SPEC.md §4.3, §6).
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

// SnapshotDir returns the absolute path to a snapshot's contents, exposed by
// ZFS at <datasetMount>/.zfs/snapshot/<snapshot>/. datasetMount is the
// filesystem mountpoint of the dataset (e.g. /mnt/bulk-pool-01/archive).
//
// The directory is stat'd so that a non-existent snapshot — which has no entry
// under .zfs/snapshot/ — yields a non-nil error rather than a path that cannot
// be read.
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

// LogicalReferenced returns the logicalreferenced property of the given ZFS
// dataset or snapshot (e.g. bulk-pool-01/archive@daily-2026-06-28) in bytes.
//
// It runs "zfs get -Hp logicalreferenced <dataset>": -H drops the header and
// emits tab-delimited fields, and -p prints the exact byte count rather than a
// human-readable size. This is the cheap feasibility pre-check input from
// SPEC.md §4.3 — it is an estimate, not the authoritative staged size.
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

// UserProperties returns the ZFS user properties set on the given dataset or
// snapshot (e.g. bulk-pool-01/.../pvc-<uuid>@snapshot-<uuid>), keyed by the full
// property name. User properties are the colon-namespaced properties (e.g.
// democratic-csi:managed_resource) that tools stamp onto datasets; native ZFS
// properties (used, compression, …) are excluded.
//
// It runs "zfs get -Hp -o property,value all <dataset>": -H drops the header and
// emits tab-delimited fields, -p prints raw values, and -o property,value selects
// just those two columns. A non-existent dataset or snapshot makes zfs exit
// non-zero, so this doubles as an existence check — the backup pipeline relies on
// that to reject a resolved snapshot that is not present on the pool (SPEC.md §4.3).
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

// parseUserProperties extracts the user properties from "zfs get -H -o
// property,value all" output. Each line is "<property>\t<value>"; a property is a
// user property when its name contains a colon (the ZFS namespace separator).
// Native properties have no colon and are skipped. ZFS property names contain no
// tab, so cutting on the first tab isolates the value, which may itself be empty.
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

// parseLogicalReferenced extracts the byte count from "zfs get -Hp" output.
//
// With -H the output is a single tab-delimited line of the form
// "<name>\tlogicalreferenced\t<value>\t<source>"; with -p the value field is a
// plain integer number of bytes. ZFS names contain no whitespace, so splitting
// on whitespace isolates the value as the third field.
func parseLogicalReferenced(out []byte) (int64, error) {
	line := strings.TrimSpace(string(out))

	fields := strings.Fields(line)
	if len(fields) < 3 {
		return 0, fmt.Errorf("unexpected zfs get output: %q", line)
	}

	value, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse logicalreferenced %q: %w", fields[2], err)
	}

	return value, nil
}
