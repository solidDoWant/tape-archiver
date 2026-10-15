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

	// Large enough to span multiple AEAD chunks.
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

			for _, identity := range identities {
				roundTripped := decryptWithAge(t, encrypted.Bytes(), identity)
				assert.Equal(t, payload, roundTripped)
			}
		})
	}
}

func TestEncryptRejectsNonPostQuantumRecipient(t *testing.T) {
	t.Parallel()

	// A valid X25519-only recipient, which SPEC §7 forbids.
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

func generateRecipient(t *testing.T) string {
	t.Helper()

	_, recipient := generateKeypair(t)

	return recipient
}

func generateKeypair(t *testing.T) (identityPath, recipient string) {
	t.Helper()

	identityPath = filepath.Join(t.TempDir(), "identity.txt")

	cmd := exec.CommandContext(t.Context(), "age-keygen", "-pq", "-o", identityPath)

	var stderr strings.Builder

	cmd.Stderr = &stderr

	require.NoError(t, cmd.Run(), "age-keygen failed: %s", stderr.String())

	contents, err := os.ReadFile(identityPath)
	require.NoError(t, err)

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
