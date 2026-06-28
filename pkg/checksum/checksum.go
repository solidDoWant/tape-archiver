// Package checksum provides SHA-256 helpers used by the Prepare, Verify, and
// Write phases to digest and validate staged files. Files are streamed through
// the hasher rather than read fully into memory, so they may be arbitrarily
// large.
package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// SHA256File streams the file at path through SHA-256 and returns its digest as
// a lowercase hex-encoded string. It returns a non-nil error if the file cannot
// be opened or read.
func SHA256File(path string) (digest string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %q: %w", path, err)
	}

	defer func() {
		if cerr := file.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing %q: %w", path, cerr)
		}
	}()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("reading %q: %w", path, err)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// Verify computes the SHA-256 digest of the file at path and compares it against
// expected. It returns nil when they match, and a non-nil error when they differ
// or when the file cannot be digested.
func Verify(path, expected string) error {
	actual, err := SHA256File(path)
	if err != nil {
		return err
	}

	if actual != expected {
		return fmt.Errorf("checksum mismatch for %q: expected %s, got %s", path, expected, actual)
	}

	return nil
}
