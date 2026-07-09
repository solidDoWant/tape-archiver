// Package testutil (this file) provides FakeOIDCProvider: a minimal,
// in-process, standards-compliant OpenID Connect identity provider used to
// exercise pkg/webauth's authorization-code flow end to end, in both
// pkg/webauth's own unit tests and cmd/web's integration tests. No real OIDC
// identity provider is available in this sandbox, and pkg/webauth must work
// against any compliant provider rather than a specific one
// (docs/web-ui-design.md §4/§6), so tests drive this fake instead of a real
// IdP or a mocked-out webauth: it serves real discovery, JWKS, an
// authorization endpoint, and a token endpoint, signing ID tokens with a
// throwaway RSA key via the same go-jose/v4 library coreos/go-oidc uses
// internally to verify them — real signature/issuer/audience/nonce
// verification runs on every test that uses it, nothing about that
// verification is mocked away.
package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// FakeOIDCProvider is a fake OIDC identity provider backed by an
// httptest.Server. Build one with NewFakeOIDCProvider; Subject/Email/Name
// (and IDTokenLifetime) may be changed after construction, before driving a
// login, to control what the next issued ID token claims.
type FakeOIDCProvider struct {
	// Server is the fake provider's HTTP server. Server.URL is both its
	// issuer URL (for OIDC discovery) and the base of its
	// authorize/token/JWKS endpoints.
	Server *httptest.Server

	// ClientID/ClientSecret are the confidential-client credentials this
	// provider expects at the token endpoint.
	ClientID     string
	ClientSecret string

	key *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]fakeAuthCode

	// Subject, Email, and Name are issued as the ID token's claims for
	// every successful exchange, standing in for "the user who
	// authenticated at the IdP". Defaulted by NewFakeOIDCProvider; tests
	// may override them before driving a login.
	Subject, Email, Name string

	// IDTokenLifetime overrides how long an issued ID token is valid for;
	// tests that need to observe expiry set this short. Defaults to one
	// hour.
	IDTokenLifetime time.Duration
}

// fakeAuthCode is what the fake authorization endpoint hands out and the
// fake token endpoint later redeems — deliberately single-use, like a real
// provider's authorization code.
type fakeAuthCode struct {
	nonce         string
	codeChallenge string
	used          bool
}

// NewFakeOIDCProvider starts a fake OIDC provider and registers cleanup to
// stop it when the test ends.
func NewFakeOIDCProvider(t testing.TB, clientID, clientSecret string) *FakeOIDCProvider {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("testutil: generate fake IdP signing key: %v", err)
	}

	idp := &FakeOIDCProvider{
		ClientID:        clientID,
		ClientSecret:    clientSecret,
		key:             key,
		codes:           map[string]fakeAuthCode{},
		Subject:         "test-user-1",
		Email:           "operator@example.com",
		Name:            "Test Operator",
		IDTokenLifetime: time.Hour,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", idp.handleDiscovery)
	mux.HandleFunc("GET /keys", idp.handleJWKS)
	mux.HandleFunc("GET /authorize", idp.handleAuthorize)
	mux.HandleFunc("POST /token", idp.handleToken)

	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Server.Close)

	return idp
}

func (idp *FakeOIDCProvider) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                idp.Server.URL,
		"authorization_endpoint":                idp.Server.URL + "/authorize",
		"token_endpoint":                        idp.Server.URL + "/token",
		"jwks_uri":                              idp.Server.URL + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email"},
	})
}

func (idp *FakeOIDCProvider) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	jwks := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{
				Key:       &idp.key.PublicKey,
				KeyID:     "test-key",
				Algorithm: "RS256",
				Use:       "sig",
			},
		},
	}

	writeJSON(w, jwks)
}

// handleAuthorize stands in for both the provider's authorization endpoint
// and the user interactively logging in there: it immediately issues a code
// and redirects back to redirect_uri, the same net effect (from the
// caller's point of view) as a real user completing a real login form.
func (idp *FakeOIDCProvider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if query.Get("client_id") != idp.ClientID {
		http.Error(w, "unknown client_id", http.StatusBadRequest)

		return
	}

	redirectURI := query.Get("redirect_uri")
	if redirectURI == "" {
		http.Error(w, "missing redirect_uri", http.StatusBadRequest)

		return
	}

	code := randomHex(randomTokenBytes)

	idp.mu.Lock()
	idp.codes[code] = fakeAuthCode{
		nonce:         query.Get("nonce"),
		codeChallenge: query.Get("code_challenge"),
	}
	idp.mu.Unlock()

	target, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "malformed redirect_uri", http.StatusBadRequest)

		return
	}

	values := target.Query()
	values.Set("code", code)
	values.Set("state", query.Get("state"))
	target.RawQuery = values.Encode()

	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (idp *FakeOIDCProvider) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)

		return
	}

	clientID, clientSecret, ok := clientCredentials(r)
	if !ok || clientID != idp.ClientID || clientSecret != idp.ClientSecret {
		http.Error(w, "invalid client credentials", http.StatusUnauthorized)

		return
	}

	if r.PostForm.Get("grant_type") != "authorization_code" {
		http.Error(w, "unsupported grant_type", http.StatusBadRequest)

		return
	}

	code := r.PostForm.Get("code")

	idp.mu.Lock()

	record, exists := idp.codes[code]
	if exists {
		if record.used {
			exists = false
		} else {
			record.used = true
			idp.codes[code] = record
		}
	}

	idp.mu.Unlock()

	if !exists {
		http.Error(w, "invalid or already-used code", http.StatusBadRequest)

		return
	}

	if record.codeChallenge != "" && !pkceMatches(record.codeChallenge, r.PostForm.Get("code_verifier")) {
		http.Error(w, "PKCE verification failed", http.StatusBadRequest)

		return
	}

	idToken, err := idp.signIDToken(record.nonce)
	if err != nil {
		http.Error(w, "failed to sign ID token", http.StatusInternalServerError)

		return
	}

	writeJSON(w, map[string]any{
		"access_token": randomHex(randomTokenBytes),
		"token_type":   "Bearer",
		"expires_in":   int(idp.IDTokenLifetime.Seconds()),
		"id_token":     idToken,
	})
}

func (idp *FakeOIDCProvider) signIDToken(nonce string) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: idp.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	if err != nil {
		return "", err
	}

	now := time.Now()

	standardClaims := jwt.Claims{
		Issuer:   idp.Server.URL,
		Subject:  idp.Subject,
		Audience: jwt.Audience{idp.ClientID},
		Expiry:   jwt.NewNumericDate(now.Add(idp.IDTokenLifetime)),
		IssuedAt: jwt.NewNumericDate(now),
	}

	extraClaims := struct {
		Nonce string `json:"nonce,omitempty"`
		Email string `json:"email,omitempty"`
		Name  string `json:"name,omitempty"`
	}{Nonce: nonce, Email: idp.Email, Name: idp.Name}

	return jwt.Signed(signer).Claims(standardClaims).Claims(extraClaims).Serialize()
}

// clientCredentials reads the client_id/client_secret confidential-client
// credentials from either HTTP Basic auth or the token request body — real
// providers accept both, so the fake does too rather than assuming which
// one a given OAuth2 client library will pick.
func clientCredentials(r *http.Request) (id, secret string, ok bool) {
	if id, secret, ok := r.BasicAuth(); ok {
		return id, secret, true
	}

	id = r.PostForm.Get("client_id")
	secret = r.PostForm.Get("client_secret")

	return id, secret, id != ""
}

// pkceMatches verifies an RFC 7636 S256 PKCE code_verifier against the
// code_challenge captured at the authorization endpoint.
func pkceMatches(codeChallenge, codeVerifier string) bool {
	sum := sha256.Sum256([]byte(codeVerifier))

	return base64.RawURLEncoding.EncodeToString(sum[:]) == codeChallenge
}

// randomTokenBytes is the byte length used for fake authorization codes and
// access tokens.
const randomTokenBytes = 16

// randomHex returns a random URL-safe string derived from n random bytes,
// used for fake authorization codes and access tokens (opaque by design —
// nothing in webauth ever parses them).
func randomHex(n int) string {
	buf := make([]byte, n)

	_, _ = rand.Read(buf)

	return base64.RawURLEncoding.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")

	_ = json.NewEncoder(w).Encode(body)
}
