package agewrap_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/agewrap"
)

// ageHeader is the start of every age file (the v1 format intro line).
var ageHeader = []byte("age-encryption.org/v1")

func TestEncryptRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		recipients int
	}{
		{name: "single recipient", recipients: 1},
		{name: "multiple recipients", recipients: 3},
	}

	// A mix of compressible runs and varied bytes, large enough to exercise the
	// streaming path through the age subprocess (multiple AEAD chunks).
	payload := make([]byte, 1<<16)
	for index := range payload {
		payload[index] = byte((index / 64) % 256)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			identities := make([]string, test.recipients)
			recipients := make([]string, test.recipients)

			for index := range identities {
				identities[index], recipients[index] = generateKeypair(t)
			}

			var encrypted bytes.Buffer

			err := agewrap.Encrypt(t.Context(), &encrypted, bytes.NewReader(payload), recipients...)
			require.NoError(t, err)

			assert.Equal(t, ageHeader, encrypted.Bytes()[:len(ageHeader)], "output must be an age file")

			// Each recipient's identity must independently decrypt to the exact
			// original bytes, using the same age binary a recoverer would use.
			for _, identity := range identities {
				roundTripped := decryptWithAge(t, encrypted.Bytes(), identity)
				assert.Equal(t, payload, roundTripped)
			}
		})
	}
}

func TestEncryptRejectsNonPostQuantumRecipient(t *testing.T) {
	t.Parallel()

	// A valid, well-formed X25519-only recipient (age1…, not age1pq1…). It must
	// be rejected by validation before age is ever invoked (SPEC §7).
	const x25519Recipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"

	tests := []struct {
		name       string
		recipients []string
	}{
		{name: "no recipients"},
		{name: "x25519 recipient", recipients: []string{x25519Recipient}},
		{name: "pq and non-pq mixed", recipients: []string{generateRecipient(t), x25519Recipient}},
		{name: "empty recipient", recipients: []string{""}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var encrypted bytes.Buffer

			err := agewrap.Encrypt(t.Context(), &encrypted, strings.NewReader("plaintext"), test.recipients...)
			require.ErrorIs(t, err, agewrap.ErrNonPostQuantumRecipient)
			assert.Empty(t, encrypted.Bytes(), "no ciphertext must be produced for a rejected recipient")
		})
	}
}

func TestEncryptContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := agewrap.Encrypt(ctx, &bytes.Buffer{}, strings.NewReader("data"), generateRecipient(t))
	require.Error(t, err)
}

// generateRecipient returns just the public recipient of a fresh post-quantum
// keypair, for tests that do not need to decrypt.
func generateRecipient(t *testing.T) string {
	t.Helper()

	_, recipient := generateKeypair(t)

	return recipient
}

// generateKeypair generates a fresh age post-quantum keypair with the real
// age-keygen binary and returns the path to the identity file and the
// corresponding recipient string.
func generateKeypair(t *testing.T) (identityPath, recipient string) {
	t.Helper()

	identityPath = filepath.Join(t.TempDir(), "identity.txt")

	cmd := exec.CommandContext(t.Context(), "age-keygen", "-pq", "-o", identityPath)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	require.NoError(t, cmd.Run(), "age-keygen failed: %s", stderr.String())

	contents, err := os.ReadFile(identityPath)
	require.NoError(t, err)

	// age-keygen records the recipient in a "# public key: age1pq1…" comment.
	const marker = "# public key: "

	for _, line := range strings.Split(string(contents), "\n") {
		if after, found := strings.CutPrefix(line, marker); found {
			recipient = strings.TrimSpace(after)

			break
		}
	}

	require.NotEmpty(t, recipient, "could not find public key in identity file")
	require.True(t, strings.HasPrefix(recipient, "age1pq1"), "expected a post-quantum recipient, got %q", recipient)

	return identityPath, recipient
}

// decryptWithAge decrypts data with the given identity file by invoking the
// bundled age binary — the same tool a recoverer uses.
func decryptWithAge(t *testing.T, data []byte, identityPath string) []byte {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "age", "-d", "-i", identityPath)
	cmd.Stdin = bytes.NewReader(data)

	var out, stderr bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &stderr

	require.NoError(t, cmd.Run(), "age decrypt failed: %s", stderr.String())

	return out.Bytes()
}
