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

// Manifest maps a disc-relative slash-separated path to its lowercase hex SHA-256.
type Manifest map[string]string

// ParseManifest parses sha256sum output (text or binary mode) into a Manifest,
// stripping a leading "./" so paths match what Verify walks.
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

// VerifyResult lists, sorted, the paths where a disc differs from its Manifest.
type VerifyResult struct {
	Mismatched []string
	Missing    []string
	Extra      []string
}

// OK reports whether the disc matches the manifest exactly.
func (r *VerifyResult) OK() bool {
	return len(r.Mismatched) == 0 && len(r.Missing) == 0 && len(r.Extra) == 0
}

// Err returns an error describing every discrepancy, or nil when OK.
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

// Verify mounts the disc read-only and compares it against manifest. The error
// reports operational failures only; content mismatches are in the VerifyResult.
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

// mountReadOnly mounts the disc read-only, via -o loop for a regular file. Requires root.
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

// verifyTree compares the regular files under root against manifest. It is
// separate from Verify so it can be unit tested without a disc.
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
