// Package checksum provides streaming SHA-256 helpers for staged files.
package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// SHA256File returns the lowercase hex SHA-256 of the file at path.
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

// Verify returns an error unless the file at path has SHA-256 digest expected.
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
