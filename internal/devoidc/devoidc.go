// Package devoidc provides FakeOIDCProvider, a minimal standards-compliant
// OpenID Connect provider with no dependency on the testing package, so
// non-test binaries such as cmd/webdevoidc can use it.
package devoidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// FakeOIDCProvider is a fake OIDC identity provider. Subject/Email/Name and
// IDTokenLifetime may be changed before a login to control the next ID token.
type FakeOIDCProvider struct {
	// Server is set when built via testutil's wrappers; its URL is the issuer.
	// Nil for NewStandaloneFakeOIDCProvider.
	Server *httptest.Server

	ClientID     string
	ClientSecret string

	key *rsa.PrivateKey

	// issuerURL is the base URL when there is no Server.
	issuerURL string

	mu    sync.Mutex
	codes map[string]fakeAuthCode

	// Subject, Email, and Name are the issued ID token's claims.
	Subject, Email, Name string

	// IDTokenLifetime defaults to one hour.
	IDTokenLifetime time.Duration
}

// fakeAuthCode is a single-use authorization code record.
type fakeAuthCode struct {
	nonce         string
	codeChallenge string
	used          bool
}

// NewStandaloneFakeOIDCProvider builds a FakeOIDCProvider plus an unstarted
// *http.Server the caller serves and shuts down. issuerURL must be the exact URL
// the server will be reachable at, since it is baked into discovery and tokens.
func NewStandaloneFakeOIDCProvider(clientID, clientSecret, issuerURL string) (*FakeOIDCProvider, *http.Server, error) {
	idp, mux, err := NewUnstarted(clientID, clientSecret)
	if err != nil {
		return nil, nil, err
	}

	idp.issuerURL = issuerURL

	return idp, &http.Server{Handler: mux}, nil
}

// NewUnstarted builds a FakeOIDCProvider and its mux without starting a server.
func NewUnstarted(clientID, clientSecret string) (*FakeOIDCProvider, *http.ServeMux, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate fake IdP signing key: %w", err)
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

	return idp, mux, nil
}

// baseURL reads Server.URL fresh on each call because e2e/web_test.go rewrites
// it after Start() for cross-container reachability.
func (idp *FakeOIDCProvider) baseURL() string {
	if idp.Server != nil {
		return idp.Server.URL
	}

	return idp.issuerURL
}

func (idp *FakeOIDCProvider) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                idp.baseURL(),
		"authorization_endpoint":                idp.baseURL() + "/authorize",
		"token_endpoint":                        idp.baseURL() + "/token",
		"jwks_uri":                              idp.baseURL() + "/keys",
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

// handleAuthorize stands in for a completed interactive login: it immediately
// issues a code and redirects back to redirect_uri.
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
		Issuer:   idp.baseURL(),
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

// clientCredentials accepts both HTTP Basic auth and form credentials, as real
// providers do.
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

const randomTokenBytes = 16

func randomHex(n int) string {
	buf := make([]byte, n)

	_, _ = rand.Read(buf)

	return base64.RawURLEncoding.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")

	_ = json.NewEncoder(w).Encode(body)
}
