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

// Mountpoint returns the filesystem mountpoint of the given ZFS dataset (e.g.
// /mnt/bulk-pool-01/archive for bulk-pool-01/archive), so the Prepare phase can
// locate the dataset's .zfs/snapshot/ tree without hardcoding the pool mount.
//
// It runs "zfs get -Hp -o value mountpoint <dataset>": -H drops the header, -p
// prints the raw value, and -o value selects just the value column. The dataset
// must be the filesystem (not a snapshot) — pass "pool/dataset", not
// "pool/dataset@snap". A dataset whose mountpoint is not a path (legacy, none) is
// returned verbatim; callers that need a real directory (SnapshotDir) surface the
// failure when the path cannot be read.
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

// Mounted reports whether the given ZFS filesystem dataset is currently mounted,
// by reading its "mounted" property. The Prepare phase uses it to refuse a raw
// dataset source whose dataset is not mounted: an unmounted dataset's mountpoint
// can survive as an ordinary "shadow" directory, and archiving that directory
// would silently certify empty or stale contents (SPEC.md §6).
//
// It runs "zfs get -Hp -o value mounted <dataset>": -H drops the header, -p
// prints the raw value, and -o value selects just the value column. Pass a
// filesystem dataset ("pool/dataset"), not a snapshot — snapshots report "-" for
// mounted, which parseMounted rejects.
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

// parseMounted maps the value of the ZFS "mounted" property to a boolean. ZFS
// reports "yes" for a mounted filesystem and "no" for an unmounted one; anything
// else (an empty value, or the "-" a snapshot or volume reports) is not a
// filesystem mount state and yields an error rather than a silent false.
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
//
// The returned map is NOT authoritative for individual property values: a user
// property value may legally embed a newline, and "all" output is newline-record
// delimited, so a crafted continuation line of the form "name:space<tab>value" is
// indistinguishable from a real record and can fabricate a property. This scrape
// is therefore used only as the raw-source existence check; any security decision
// (e.g. the democratic-csi ownership guard) must read the specific property by
// name via UserProperty, which is structurally immune to that ambiguity.
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

// UserProperty returns the value of a single named ZFS user property (e.g.
// democratic-csi:managed_resource) on the given dataset or snapshot. Unlike
// UserProperties, it reads exactly one property by name and so cannot be spoofed
// by a continuation line embedded in some other property's value — there is no
// multi-record parsing to confuse. It is the reader the ownership guard uses.
//
// It runs "zfs get -Hp -o value <property> <dataset>": -H drops the header, -p
// prints the raw value, and -o value selects just the value column. zfs emits the
// property's value on a single line, or "-" when the property is unset on the
// dataset; the value is returned trimmed. A non-existent dataset or snapshot makes
// zfs exit non-zero, so this preserves the existence semantics of UserProperties.
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

// Hold places a ZFS user hold tagged tag on the given snapshot (e.g.
// bulk-pool-01/archive@daily-2026-06-28), pinning it against destruction: a
// snapshot with any hold cannot be removed by `zfs destroy` until every hold is
// released. A backup run holds each of its source snapshots for the run's
// duration so external pruning cannot delete a snapshot mid-run (SPEC.md §4.3).
//
// It runs "zfs hold <tag> <snapshot>". The operation is idempotent: when the tag
// is already present on the snapshot zfs exits non-zero reporting "tag already
// exists", which is treated as success so an activity retry that re-holds is a
// harmless no-op.
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

// Release removes the ZFS user hold tagged tag from the given snapshot,
// unpinning it so it can be destroyed once no other holds remain. A run releases
// its holds on every exit path (success, failure, cancellation) so a completed
// run never leaves a snapshot pinned (SPEC.md §4.3).
//
// It runs "zfs release <tag> <snapshot>". It is tolerant of an already-absent
// hold: when zfs exits non-zero reporting "no such tag" (the hold was never
// placed, or was already released) that is treated as success, so releasing on
// an exit path where the hold was never taken is a harmless no-op.
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

// Holds returns the user-hold tags currently placed on the given snapshot, so a
// caller can confirm a hold is present (or gone). An unheld snapshot yields an
// empty list, not an error.
//
// It runs "zfs holds -H <snapshot>": -H drops the header and emits tab-delimited
// fields (NAME, TAG, TIMESTAMP) one line per hold. The tags are extracted by the
// pure parseHolds helper.
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

// parseHolds extracts the hold tags from "zfs holds -H" output. Each line is
// "<snapshot>\t<tag>\t<timestamp>"; -H tab-delimits the three columns, so the tag
// is field 2. The timestamp column may itself contain spaces, but never a tab, so
// splitting on tab isolates the tag cleanly. Lines with fewer than two fields
// (none expected) are skipped, and empty output yields an empty list.
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
// plain integer number of bytes. OpenZFS name components may legally contain
// spaces (but never a tab), so the line must be cut on tabs, not on arbitrary
// whitespace: a space in the name would otherwise shift the field indices and
// yield a parse error or — when the third whitespace token happens to be numeric
// (e.g. "tank/media disc 2") — a silently wrong byte count. The value is field 2.
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
