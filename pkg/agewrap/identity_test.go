package agewrap_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/pkg/agewrap"
)

// TestRecipientFromIdentity checks that the recipient derived from a private
// identity matches the recipient age-keygen recorded when it generated the
// keypair — the equality the Report phase relies on to prove an escrowed identity
// can decrypt what the recipients encrypted.
func TestRecipientFromIdentity(t *testing.T) {
	t.Parallel()

	identityPath, recipient := generateKeypair(t)

	contents, err := os.ReadFile(identityPath)
	require.NoError(t, err)

	derived, err := agewrap.RecipientFromIdentity(t.Context(), string(contents))
	require.NoError(t, err)

	assert.Equal(t, recipient, derived)
}

// TestRecipientFromIdentityRejectsEmpty checks an empty or whitespace identity is
// rejected before age-keygen is invoked.
func TestRecipientFromIdentityRejectsEmpty(t *testing.T) {
	t.Parallel()

	for _, identity := range []string{"", "   \n\t"} {
		_, err := agewrap.RecipientFromIdentity(t.Context(), identity)
		require.Error(t, err)
	}
}

// TestRecipientFromIdentityRejectsInvalid checks that a string that is not a
// valid age identity yields an error rather than a bogus recipient.
func TestRecipientFromIdentityRejectsInvalid(t *testing.T) {
	t.Parallel()

	_, err := agewrap.RecipientFromIdentity(t.Context(), "AGE-SECRET-KEY-PQ-1NOTAREALIDENTITY")
	require.Error(t, err)
}

// TestGenerateIdentity checks that GenerateIdentity produces a post-quantum
// identity/recipient pair, that the recipient is exactly what
// RecipientFromIdentity independently derives from the returned identity
// (the equality GenerateIdentity's own doc comment promises), and that the
// identity round-trips as a valid decryption key for its recipient — the
// same round-trip the web UI's config page relies on when it inserts the
// generated recipient into a run config.
func TestGenerateIdentity(t *testing.T) {
	t.Parallel()

	identity, recipient, err := agewrap.GenerateIdentity(t.Context())
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(recipient, "age1pq1"), "recipient must be post-quantum: %q", recipient)
	assert.True(t, strings.HasPrefix(identity, "AGE-SECRET-KEY-PQ-1"), "identity must be post-quantum: %q", identity)

	derived, err := agewrap.RecipientFromIdentity(t.Context(), identity)
	require.NoError(t, err)
	assert.Equal(t, recipient, derived)

	var encrypted bytes.Buffer
	require.NoError(t, agewrap.Encrypt(t.Context(), &encrypted, strings.NewReader("roundtrip"), recipient))

	identityPath := filepath.Join(t.TempDir(), "identity.txt")
	require.NoError(t, os.WriteFile(identityPath, []byte(identity), 0o600))

	roundTripped := decryptWithAge(t, encrypted.Bytes(), identityPath)
	assert.Equal(t, "roundtrip", string(roundTripped))
}

// TestGenerateIdentityIsFreshEveryCall checks two calls never return the same
// keypair — GenerateIdentity must never cache or reuse a previously generated
// identity, matching the "displayed once, never retrievable again" semantics
// the web UI's config page relies on.
func TestGenerateIdentityIsFreshEveryCall(t *testing.T) {
	t.Parallel()

	firstIdentity, firstRecipient, err := agewrap.GenerateIdentity(t.Context())
	require.NoError(t, err)

	secondIdentity, secondRecipient, err := agewrap.GenerateIdentity(t.Context())
	require.NoError(t, err)

	assert.NotEqual(t, firstIdentity, secondIdentity)
	assert.NotEqual(t, firstRecipient, secondRecipient)
}
