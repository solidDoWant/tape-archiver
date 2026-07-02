package agewrap_test

import (
	"os"
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
