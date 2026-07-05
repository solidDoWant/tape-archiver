package optical

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/solidDoWant/tape-archiver/pkg/checksum"
)

// Manifest maps a disc-relative slash-separated path to its expected lowercase
// hex SHA-256 digest. It is the set of files a burned disc must contain, and
// their digests, for Verify to consider the disc good.
type Manifest map[string]string

// ParseManifest parses the standard sha256sum manifest format — one
// "<hex-digest>  <path>" line per file — into a Manifest. The two-space (text)
// and " *" (binary) separators sha256sum emits are both accepted, and a leading
// "./" on the path is stripped so paths match the relative paths Verify walks.
// Blank lines are ignored. It returns an error on a malformed line or a duplicate
// path.
func ParseManifest(r io.Reader) (Manifest, error) {
	manifest := make(Manifest)
	scanner := bufio.NewScanner(r)

	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}

		digest, rest, found := strings.Cut(text, " ")
		if !found {
			return nil, fmt.Errorf("optical: malformed manifest line %d: %q", line, text)
		}

		if len(digest) != 64 {
			return nil, fmt.Errorf("optical: malformed manifest line %d: %q is not a SHA-256 digest", line, digest)
		}

		// sha256sum separates digest and name with two spaces (text mode) or a
		// space and '*' (binary mode); trim the residual separator character.
		name := strings.TrimPrefix(strings.TrimSpace(rest), "*")
		name = strings.TrimPrefix(strings.TrimSpace(name), "./")

		if name == "" {
			return nil, fmt.Errorf("optical: malformed manifest line %d: empty path", line)
		}

		if _, exists := manifest[name]; exists {
			return nil, fmt.Errorf("optical: duplicate manifest entry for %q", name)
		}

		manifest[name] = strings.ToLower(digest)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("optical: reading manifest: %w", err)
	}

	return manifest, nil
}

// VerifyResult is the outcome of comparing a burned disc's contents against a
// Manifest. A disc verifies only when all three slices are empty (OK). Each slice
// is sorted for stable, readable reporting.
type VerifyResult struct {
	// Mismatched holds paths present on the disc and in the manifest whose
	// SHA-256 digests differ.
	Mismatched []string
	// Missing holds paths in the manifest that are absent from the disc.
	Missing []string
	// Extra holds regular files on the disc that the manifest does not list.
	Extra []string
}

// OK reports whether the disc matches the manifest exactly — no mismatched,
// missing, or extra files.
func (r *VerifyResult) OK() bool {
	return len(r.Mismatched) == 0 && len(r.Missing) == 0 && len(r.Extra) == 0
}

// Err returns a non-nil error describing every discrepancy when the disc does not
// verify, and nil when it does. Callers that want the verification failure as an
// error (the common case) use this; callers that want to inspect the discrepancy
// lists read the fields directly.
func (r *VerifyResult) Err() error {
	if r.OK() {
		return nil
	}

	var parts []string

	if len(r.Mismatched) > 0 {
		parts = append(parts, fmt.Sprintf("mismatched: %s", strings.Join(r.Mismatched, ", ")))
	}

	if len(r.Missing) > 0 {
		parts = append(parts, fmt.Sprintf("missing: %s", strings.Join(r.Missing, ", ")))
	}

	if len(r.Extra) > 0 {
		parts = append(parts, fmt.Sprintf("extra: %s", strings.Join(r.Extra, ", ")))
	}

	return fmt.Errorf("optical: disc does not match manifest (%s)", strings.Join(parts, "; "))
}

// Verify mounts this disc read-only and checks its contents against manifest,
// returning a VerifyResult describing any mismatched, missing, or extra files. It
// mounts to a temporary directory, compares, and unmounts before returning (even
// on error). A returned error signals an operational failure (mount or read); a
// content mismatch is reported through the VerifyResult, whose Err/OK methods the
// caller uses to decide pass/fail.
func (d *Disc) Verify(ctx context.Context, manifest Manifest) (result *VerifyResult, err error) {
	mountpoint, err := os.MkdirTemp("", "optical-verify-")
	if err != nil {
		return nil, fmt.Errorf("optical: creating mountpoint: %w", err)
	}
	defer func() {
		if rerr := os.RemoveAll(mountpoint); rerr != nil && err == nil {
			err = fmt.Errorf("optical: removing mountpoint %s: %w", mountpoint, rerr)
		}
	}()

	if err := d.mountReadOnly(ctx, mountpoint); err != nil {
		return nil, err
	}
	defer func() {
		if uerr := unmount(ctx, mountpoint); uerr != nil && err == nil {
			err = uerr
		}
	}()

	return verifyTree(mountpoint, manifest)
}

// mountReadOnly mounts this disc's filesystem read-only at mountpoint. A regular
// file backing (the stdio pseudo-disc in tests) is mounted through a loop device
// (-o loop); a block device (a real optical drive, or a loop device) is mounted
// directly. Mounting an ISO 9660 filesystem requires privilege, which the
// integration target provides (it runs under sudo, like the LTFS path).
func (d *Disc) mountReadOnly(ctx context.Context, mountpoint string) error {
	options := "ro"

	info, err := os.Stat(d.device)
	if err != nil {
		return fmt.Errorf("optical: stat %s: %w", d.device, err)
	}

	if info.Mode().IsRegular() {
		options += ",loop"
	}

	cmd := exec.CommandContext(ctx, "mount", "-t", "iso9660", "-o", options, d.device, mountpoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("optical: mounting %s read-only: %w: %s", d.device, err, msg)
		}

		return fmt.Errorf("optical: mounting %s read-only: %w", d.device, err)
	}

	return nil
}

// unmount unmounts the filesystem at mountpoint.
func unmount(ctx context.Context, mountpoint string) error {
	cmd := exec.CommandContext(ctx, "umount", mountpoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("optical: unmounting %s: %w: %s", mountpoint, err, msg)
		}

		return fmt.Errorf("optical: unmounting %s: %w", mountpoint, err)
	}

	return nil
}

// verifyTree walks the regular files under root and compares them against
// manifest, returning the mismatched, missing, and extra paths. It is a pure
// function of the filesystem tree and the manifest — no disc, mount, or xorriso —
// so the verification logic (the highest-value, most bug-prone path) is unit
// tested directly. Paths are reported disc-relative with slash separators.
func verifyTree(root string, manifest Manifest) (*VerifyResult, error) {
	seen := make(map[string]bool, len(manifest))
	result := &VerifyResult{}

	walkErr := filepath.WalkDir(root, func(pathName string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(root, pathName)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)
		seen[rel] = true

		expected, listed := manifest[rel]
		if !listed {
			result.Extra = append(result.Extra, rel)

			return nil
		}

		digest, err := checksum.SHA256File(pathName)
		if err != nil {
			return err
		}

		if digest != expected {
			result.Mismatched = append(result.Mismatched, rel)
		}

		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("optical: reading back disc contents: %w", walkErr)
	}

	for name := range manifest {
		if !seen[name] {
			result.Missing = append(result.Missing, name)
		}
	}

	sort.Strings(result.Mismatched)
	sort.Strings(result.Missing)
	sort.Strings(result.Extra)

	return result, nil
}
