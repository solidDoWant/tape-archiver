package webauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// sessionClaims is the plaintext payload of the session cookie.
type sessionClaims struct {
	Subject   string `json:"sub"`
	Email     string `json:"email,omitempty"`
	Name      string `json:"name,omitempty"`
	ExpiresAt int64  `json:"exp"`
}

// stateClaims is the plaintext payload of the short-lived login-state cookie;
// with no server-side store, it is how login state survives the IdP redirect.
type stateClaims struct {
	State        string `json:"state"`
	Nonce        string `json:"nonce"`
	PKCEVerifier string `json:"pkce_verifier"`
	RedirectPath string `json:"redirect_path"`
	ExpiresAt    int64  `json:"exp"`
}

// encrypt seals v as JSON with AES-256-GCM. purpose is the AAD, binding the
// ciphertext to one cookie kind so session and state cookies can't be swapped.
func (a *Authenticator) encrypt(purpose string, v any) (string, error) {
	plaintext, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("webauth: encode cookie payload: %w", err)
	}

	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("webauth: generate cookie nonce: %w", err)
	}

	ciphertext := a.gcm.Seal(nonce, nonce, plaintext, []byte(purpose))

	return base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

// decrypt is the inverse of encrypt. Every failure returns the same opaque
// error so a crafted cookie can't probe which check it failed.
func (a *Authenticator) decrypt(purpose string, value string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return errors.New("webauth: invalid cookie encoding")
	}

	nonceSize := a.gcm.NonceSize()
	if len(raw) < nonceSize {
		return errors.New("webauth: invalid cookie")
	}

	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]

	plaintext, err := a.gcm.Open(nil, nonce, ciphertext, []byte(purpose))
	if err != nil {
		return errors.New("webauth: invalid cookie")
	}

	if err := json.Unmarshal(plaintext, v); err != nil {
		return errors.New("webauth: invalid cookie")
	}

	return nil
}

type identityContextKey struct{}

func withIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFromContext returns the Identity attached by Authenticator.Wrap's
// session middleware; it always succeeds behind gated routes.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(Identity)

	return identity, ok
}
