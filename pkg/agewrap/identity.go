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

// GenerateIdentity generates a fresh age native post-quantum identity (hybrid
// ML-KEM-768 + X25519, `age-keygen -pq`) and its recipient, for the web UI's
// config-page keypair-generation button (pkg/runsapi's POST /api/age/keygen).
// SPEC §7 mandates post-quantum recipients only, matching Encrypt's own
// pqRecipientPrefix check — a plain X25519 `age-keygen` (no -pq) identity
// would be rejected by Encrypt, so this always uses the -pq form.
//
// It shells out to the bundled age-keygen binary — the exact tool that ships
// on the recovery disc (SPEC §7, §10) — so a generated identity is produced
// by the same implementation a future recoverer uses to decrypt with it.
// GenerateIdentity never writes the identity to disk (no -o flag; it is
// generated to this process's memory only) and never logs it — callers must
// uphold the same discipline: display it to the operator exactly once and
// never persist or log it server-side.
//
// The recipient is derived from the generated identity via
// RecipientFromIdentity (age-keygen -y) rather than parsed out of
// age-keygen -pq's own "# public key: ..." comment line, so the two can
// never silently drift — the recipient returned here is always exactly what
// age itself would derive from the returned identity.
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

	// age-keygen -pq's stdout is "# created: ..." and "# public key: ..."
	// comment lines followed by the AGE-SECRET-KEY-PQ-1... identity line on
	// its own; take the first non-comment, non-empty line as the identity.
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

	recipient, err = RecipientFromIdentity(ctx, identity)
	if err != nil {
		return "", "", fmt.Errorf("derive recipient for generated identity: %w", err)
	}

	return identity, recipient, nil
}
