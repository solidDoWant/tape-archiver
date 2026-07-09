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
	"fmt"
	"net"
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
	// Server is the fake provider's HTTP server when constructed via
	// NewFakeOIDCProvider/NewFakeOIDCProviderOn. Server.URL is both its
	// issuer URL (for OIDC discovery) and the base of its
	// authorize/token/JWKS endpoints. Nil when constructed via
	// NewStandaloneFakeOIDCProvider — use baseURL() internally, and the
	// issuerURL passed to that constructor externally, instead of reading
	// this field directly in that case.
	Server *httptest.Server

	// ClientID/ClientSecret are the confidential-client credentials this
	// provider expects at the token endpoint.
	ClientID     string
	ClientSecret string

	key *rsa.PrivateKey

	// issuerURL is the provider's base URL when it is NOT backed by Server
	// (i.e. built by NewStandaloneFakeOIDCProvider for a long-lived,
	// non-test caller — see cmd/webdevoidc). baseURL() prefers Server.URL
	// whenever Server is set, so this field is only ever consulted in the
	// standalone case.
	issuerURL string

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

	idp, server := newUnstartedFakeOIDCProvider(t, clientID, clientSecret)
	server.Start()
	t.Cleanup(server.Close)

	return idp
}

// NewFakeOIDCProviderOn behaves exactly like NewFakeOIDCProvider, but binds
// the fake provider's HTTP server to the given listener instead of an
// ephemeral loopback port. e2e/web_test.go uses this to bind the fake IdP to
// the kind bridge's gateway IP, so it is reachable both from pods inside the
// kind cluster (cmd/web's own OIDC discovery/token-exchange calls) and from
// the host browser (the /authorize redirect hop a real browser follows) —
// the same reachability trick the existing e2e suite's mock webhook uses
// (e2e/harness_test.go's startWebhook). Ownership of listener transfers to
// the returned FakeOIDCProvider's httptest.Server, which closes it via
// t.Cleanup.
func NewFakeOIDCProviderOn(t testing.TB, clientID, clientSecret string, listener net.Listener) *FakeOIDCProvider {
	t.Helper()

	idp, server := newUnstartedFakeOIDCProvider(t, clientID, clientSecret)

	// Discard the default ephemeral loopback listener httptest.NewUnstartedServer
	// already created, and start on the caller's listener instead — the
	// documented way to bind an httptest.Server to a specific address.
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	return idp
}

// newUnstartedFakeOIDCProvider builds the fake provider and its mux, wired
// into an unstarted httptest.Server so both NewFakeOIDCProvider (default
// loopback listener) and NewFakeOIDCProviderOn (caller-supplied listener)
// share one construction path. It is a thin testing.TB-aware wrapper over
// buildFakeOIDCProvider, which does the actual (TB-free) construction work —
// see that function's doc comment for why the split exists.
func newUnstartedFakeOIDCProvider(t testing.TB, clientID, clientSecret string) (*FakeOIDCProvider, *httptest.Server) {
	t.Helper()

	idp, mux, err := buildFakeOIDCProvider(clientID, clientSecret)
	if err != nil {
		t.Fatalf("testutil: %v", err)
	}

	server := httptest.NewUnstartedServer(mux)
	idp.Server = server

	return idp, server
}

// NewStandaloneFakeOIDCProvider builds a FakeOIDCProvider usable outside a Go
// test binary. NewFakeOIDCProvider/NewFakeOIDCProviderOn cannot serve this:
// both require a testing.TB, and testing.TB has an unexported method
// specifically so nothing outside package testing can implement it — a
// long-lived, non-test caller (cmd/webdevoidc, for `make web-dev`) has no
// value to pass. This returns a plain, unstarted *http.Server instead of an
// httptest.Server: the caller binds its own listener and Serve()s it, and is
// responsible for shutting it down (nothing here registers a t.Cleanup or
// calls t.Fatalf — errors are returned instead).
//
// issuerURL must be the exact base URL the caller will make this server
// reachable at (e.g. "http://127.0.0.1:9998") — it is baked into OIDC
// discovery responses and signed ID tokens' issuer claim up front, before the
// caller has necessarily bound a listener. This sidesteps the
// ephemeral-port chicken-and-egg NewFakeOIDCProvider/NewFakeOIDCProviderOn
// avoid differently (via a real httptest.Server, which learns its own address
// from the listener it starts on): a long-lived dev command instead picks a
// fixed, well-known local port up front and passes the matching URL here.
func NewStandaloneFakeOIDCProvider(clientID, clientSecret, issuerURL string) (*FakeOIDCProvider, *http.Server, error) {
	idp, mux, err := buildFakeOIDCProvider(clientID, clientSecret)
	if err != nil {
		return nil, nil, err
	}

	idp.issuerURL = issuerURL

	return idp, &http.Server{Handler: mux}, nil
}

// buildFakeOIDCProvider does the actual construction work shared by all three
// constructors above (generate the signing key, default the claims, wire the
// mux) without depending on testing.TB, so NewStandaloneFakeOIDCProvider can
// use it directly and the two testing.TB-based constructors can wrap it with
// t.Fatalf on error.
func buildFakeOIDCProvider(clientID, clientSecret string) (*FakeOIDCProvider, *http.ServeMux, error) {
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

// baseURL returns the provider's issuer/base URL, preferring the live
// Server.URL (which e2e/web_test.go mutates after Start() to rebind the
// advertised address for cross-container reachability — reading it fresh
// here, rather than caching it at construction time, preserves that) and
// falling back to the fixed issuerURL set by NewStandaloneFakeOIDCProvider
// when there is no Server at all.
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
