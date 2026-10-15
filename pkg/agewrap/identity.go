package agewrap

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// RecipientFromIdentity derives the public recipient for an age identity via
// `age-keygen -y`. The identity is passed on stdin so it never touches disk.
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

	for _, line := range strings.Split(stdout.String(), "\n") {
		if recipient := strings.TrimSpace(line); recipient != "" {
			return recipient, nil
		}
	}

	return "", fmt.Errorf("age-keygen produced no recipient for the given identity")
}

// GenerateIdentity generates a fresh post-quantum identity (`age-keygen -pq`,
// as SPEC §7 requires) and its recipient. The identity is never written to disk
// or logged; callers must likewise never persist or log it.
func GenerateIdentity(ctx context.Context) (identity, recipient string, err error) {
	cmd := exec.CommandContext(ctx, "age-keygen", "-pq")

	var stdout, stderr strings.Builder

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", "", fmt.Errorf("%s: %w: %s", cmd, err, msg)
		}

		return "", "", fmt.Errorf("%s: %w", cmd, err)
	}

	// The identity is the first non-comment line.
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		identity = line

		break
	}

	if identity == "" {
		return "", "", fmt.Errorf("age-keygen -pq produced no identity")
	}

	// Derive rather than parse the "# public key" line, so the two cannot drift.
	recipient, err = RecipientFromIdentity(ctx, identity)
	if err != nil {
		return "", "", fmt.Errorf("derive recipient for generated identity: %w", err)
	}

	return identity, recipient, nil
}
