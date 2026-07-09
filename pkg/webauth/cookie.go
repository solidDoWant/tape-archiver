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

// stateClaims is the plaintext payload of the short-lived login-state
// cookie, carrying everything handleCallback needs that cannot otherwise
// survive the redirect through the IdP (there is no server-side session
// store to stash it in — see the package doc comment).
type stateClaims struct {
	State        string `json:"state"`
	Nonce        string `json:"nonce"`
	PKCEVerifier string `json:"pkce_verifier"`
	RedirectPath string `json:"redirect_path"`
	ExpiresAt    int64  `json:"exp"`
}

// encrypt marshals v as JSON and seals it with AES-256-GCM under purpose as
// additional authenticated data, returning a URL-safe cookie value. purpose
// binds a ciphertext to the cookie kind it was created for (see
// decrypt) — decrypting a session cookie's value with the state purpose (or
// vice versa) fails authentication even though both use the same key.
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

// decrypt is the inverse of encrypt: it decodes, authenticates, and decrypts
// value (a cookie's Value), then JSON-unmarshals the plaintext into v. Any
// failure along the way — malformed base64, too-short ciphertext, a failed
// GCM authentication tag (wrong key, wrong purpose, or a tampered value),
// bad JSON — is returned as a single opaque error: callers treat every
// failure mode identically (an invalid cookie, full stop), never
// distinguishing "tampered" from "wrong purpose" from "corrupt", so a
// crafted cookie can't be used to probe which failure mode it hit.
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

// identityContextKey is an unexported type so context values set by this
// package can never collide with a key from another package (the standard
// Go context-key idiom).
type identityContextKey struct{}

// withIdentity returns a copy of ctx carrying identity, attached by
// requireSession for every gated request.
func withIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFromContext returns the authenticated Identity attached to ctx by
// the session middleware (Authenticator.Wrap), if any. Handlers mounted
// behind Wrap's gated routes (i.e. everything except /auth/login,
// /auth/callback, /auth/logout) can rely on this always succeeding.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(Identity)

	return identity, ok
}
