// Package agewrap wraps the bundled age binary to encrypt streams to
// post-quantum recipients (SPEC §7).
package agewrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// pqRecipientPrefix marks an age native post-quantum recipient; SPEC §7
// forbids X25519-only recipients.
const pqRecipientPrefix = "age1pq1"

// ErrNonPostQuantumRecipient is returned, before age is invoked, when Encrypt
// gets no recipients or a non-post-quantum one.
var ErrNonPostQuantumRecipient = errors.New("recipient is not an age post-quantum recipient (age1pq1…)")

// Encrypt age-encrypts src to dst for recipients, all of which must be
// post-quantum, using the same age binary that ships on the recovery disc.
func Encrypt(ctx context.Context, dst io.Writer, src io.Reader, recipients ...string) error {
	if len(recipients) == 0 {
		return fmt.Errorf("%w: no recipients given", ErrNonPostQuantumRecipient)
	}

	// Validate every recipient before age sees any plaintext.
	args := []string{"-e"}

	for _, recipient := range recipients {
		if !strings.HasPrefix(recipient, pqRecipientPrefix) {
			return fmt.Errorf("%w: %q", ErrNonPostQuantumRecipient, recipient)
		}

		args = append(args, "-r", recipient)
	}

	cmd := exec.CommandContext(ctx, "age", args...)
	cmd.Stdin = src
	cmd.Stdout = dst

	var stderr strings.Builder

	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return fmt.Errorf("%s: %w", cmd, err)
	}

	return nil
}
