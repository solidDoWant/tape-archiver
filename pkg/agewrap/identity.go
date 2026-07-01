package agewrap

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// RecipientFromIdentity derives the public recipient (age1…) for an age private
// identity (AGE-SECRET-KEY-…) by shelling out to `age-keygen -y`, the same tool
// the recovery disc ships (SPEC §7). The identity is fed on stdin so it never
// touches disk.
//
// It is used by the Report phase to verify the escrowed private identity actually
// matches a configured recipient before embedding it in the report and ISO —
// escrowing a key that cannot decrypt the archives would defeat the escrow
// (SPEC §7). It returns an error if the identity is empty, if age-keygen cannot
// run, or if it produces no recipient.
func RecipientFromIdentity(ctx context.Context, identity string) (string, error) {
	if strings.TrimSpace(identity) == "" {
		return "", fmt.Errorf("agewrap: empty age identity")
	}

	cmd := exec.CommandContext(ctx, "age-keygen", "-y")
	cmd.Stdin = strings.NewReader(identity)

	var stdout, stderr strings.Builder

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return "", fmt.Errorf("%s: %w", cmd, err)
	}

	// A single identity yields a single recipient line; take the first non-empty
	// line so a trailing newline (or an echoed comment) does not leak in.
	for _, line := range strings.Split(stdout.String(), "\n") {
		if recipient := strings.TrimSpace(line); recipient != "" {
			return recipient, nil
		}
	}

	return "", fmt.Errorf("age-keygen produced no recipient for the given identity")
}
