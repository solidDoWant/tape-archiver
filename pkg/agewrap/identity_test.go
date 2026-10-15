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

func TestRecipientFromIdentity(t *testing.T) {
	t.Parallel()

	identityPath, recipient := generateKeypair(t)

	contents, err := os.ReadFile(identityPath)
	require.NoError(t, err)

	derived, err := agewrap.RecipientFromIdentity(t.Context(), string(contents))
	require.NoError(t, err)

	assert.Equal(t, recipient, derived)
}

func TestRecipientFromIdentityRejectsEmpty(t *testing.T) {
	t.Parallel()

	for _, identity := range []string{"", "   \n\t"} {
		_, err := agewrap.RecipientFromIdentity(t.Context(), identity)
		require.Error(t, err)
	}
}

func TestRecipientFromIdentityRejectsInvalid(t *testing.T) {
	t.Parallel()

	_, err := agewrap.RecipientFromIdentity(t.Context(), "AGE-SECRET-KEY-PQ-1NOTAREALIDENTITY")
	require.Error(t, err)
}

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

func TestGenerateIdentityIsFreshEveryCall(t *testing.T) {
	t.Parallel()

	firstIdentity, firstRecipient, err := agewrap.GenerateIdentity(t.Context())
	require.NoError(t, err)

	secondIdentity, secondRecipient, err := agewrap.GenerateIdentity(t.Context())
	require.NoError(t, err)

	assert.NotEqual(t, firstIdentity, secondIdentity)
	assert.NotEqual(t, firstRecipient, secondRecipient)
}
