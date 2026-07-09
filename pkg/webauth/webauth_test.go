package webauth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
)

// testConfig builds a Config wired to a fresh fake OIDC provider, plus a
// 32-byte session key — everything New needs to build a real Authenticator
// against an in-process provider.
func testConfig(t *testing.T, idp *testutil.FakeOIDCProvider) Config {
	t.Helper()

	return Config{
		IssuerURL:    idp.Server.URL,
		ClientID:     idp.ClientID,
		ClientSecret: idp.ClientSecret,
		RedirectURL:  "http://app.example.com/auth/callback",
		SessionKey:   testSessionKey(t),
	}
}

func testSessionKey(t *testing.T) []byte {
	t.Helper()

	key := make([]byte, sessionKeyLen)
	_, err := rand.Read(key)
	require.NoError(t, err)

	return key
}

// echoHandler is a stand-in for pkg/webserver's SPA+API handler: it reports
// the request path and, when a session was attached to the context, the
// authenticated Identity — enough for tests to assert both "the gate let
// this request through" and "the gate attached the right identity".
func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		identity, _ := IdentityFromContext(r.Context())

		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":     r.URL.Path,
			"identity": identity,
		})
	})
}

// newGatedServer builds an httptest.Server serving authenticator.Wrap over
// echoHandler(), standing in for cmd/web's real handler chain
// (webauth.Wrap(webserver.NewHandler(...))) without depending on
// pkg/webserver or pkg/runsapi.
func newGatedServer(t *testing.T, authenticator *Authenticator) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(authenticator.Wrap(echoHandler()))
	t.Cleanup(server.Close)

	return server
}

// newGatedServerWithRealRedirect is like newGatedServer, but points the
// Authenticator's RedirectURL at the server's own real loopback address
// instead of a placeholder. Only tests that drive the login flow through an
// actual HTTP redirect chain (the fake IdP's /authorize handler 302s the
// client straight to RedirectURL — see testutil.FakeOIDCProvider) need this;
// everything else can use testConfig + New + newGatedServer, since a
// redirect target that is never actually dialed does not need to resolve.
func newGatedServerWithRealRedirect(t *testing.T, idp *testutil.FakeOIDCProvider) (*httptest.Server, *Authenticator) {
	t.Helper()

	var handler http.Handler

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))

	cfg := testConfig(t, idp)
	cfg.RedirectURL = "http://" + server.Listener.Addr().String() + "/auth/callback"

	authenticator, err := New(t.Context(), cfg)
	require.NoError(t, err)

	handler = authenticator.Wrap(echoHandler())

	server.Start()
	t.Cleanup(server.Close)

	return server, authenticator
}

func TestNew_validatesConfig(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")

	tests := []struct {
		name      string
		mutate    func(cfg *Config)
		assertErr require.ErrorAssertionFunc
	}{
		{name: "valid config", mutate: func(*Config) {}},
		{name: "missing issuer URL", mutate: func(cfg *Config) { cfg.IssuerURL = "" }, assertErr: require.Error},
		{name: "missing client ID", mutate: func(cfg *Config) { cfg.ClientID = "" }, assertErr: require.Error},
		{name: "missing client secret", mutate: func(cfg *Config) { cfg.ClientSecret = "" }, assertErr: require.Error},
		{name: "missing redirect URL", mutate: func(cfg *Config) { cfg.RedirectURL = "" }, assertErr: require.Error},
		{name: "short session key", mutate: func(cfg *Config) { cfg.SessionKey = []byte("too-short") }, assertErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			cfg := testConfig(t, idp)
			test.mutate(&cfg)

			_, err := New(t.Context(), cfg)
			test.assertErr(t, err)
		})
	}
}

func TestParseSessionKey(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		assertErr require.ErrorAssertionFunc
	}{
		{name: "valid 32-byte key", input: base64.StdEncoding.EncodeToString(make([]byte, 32))},
		{name: "wrong length", input: base64.StdEncoding.EncodeToString(make([]byte, 16)), assertErr: require.Error},
		{name: "not base64", input: "not-valid-base64!!!", assertErr: require.Error},
		{name: "empty", input: "", assertErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			key, err := ParseSessionKey(test.input)
			test.assertErr(t, err)

			if err != nil {
				return
			}

			assert.Len(t, key, sessionKeyLen)
		})
	}
}

// TestUnauthenticatedRequests_areRejected covers the split gating decision:
// a protected API route 401s (JSON body) rather than redirecting, since a
// fetch()/XHR caller cannot follow a redirect into an HTML login page
// usefully; a protected page route redirects to /auth/login instead, since
// that is what a browser navigation needs.
func TestUnauthenticatedRequests_areRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	t.Run("API route without a session returns 401", func(t *testing.T) {
		resp, err := client.Get(server.URL + "/api/runs")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	})

	t.Run("api/me without a session returns 401", func(t *testing.T) {
		resp, err := client.Get(server.URL + "/api/me")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("page route without a session redirects to login", func(t *testing.T) {
		resp, err := client.Get(server.URL + "/runs/some-run-id")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusFound, resp.StatusCode)
		location := resp.Header.Get("Location")
		assert.Contains(t, location, "/auth/login")
		assert.Contains(t, location, "redirect=%2Fruns%2Fsome-run-id")
	})

	t.Run("root route without a session redirects to login", func(t *testing.T) {
		resp, err := client.Get(server.URL + "/")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusFound, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Location"), "/auth/login")
	})
}

// TestLoginRedirectsToProvider covers "GET /auth/login redirects to the
// provider's authorization endpoint with the configured client ID and
// redirect URL", without following the redirect (that is exercised
// end-to-end by TestFullLoginFlow below).
func TestLoginRedirectsToProvider(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(server.URL + "/auth/login")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusFound, resp.StatusCode)

	location, err := resp.Location()
	require.NoError(t, err)

	assert.Equal(t, idp.Server.URL+"/authorize", location.Scheme+"://"+location.Host+location.Path)
	assert.Equal(t, "client-1", location.Query().Get("client_id"))
	assert.Equal(t, "http://app.example.com/auth/callback", location.Query().Get("redirect_uri"))
	assert.NotEmpty(t, location.Query().Get("state"))
	assert.NotEmpty(t, location.Query().Get("code_challenge"))
	assert.Equal(t, "S256", location.Query().Get("code_challenge_method"))

	// A state cookie must have been set for the callback to consume.
	var sawStateCookie bool

	for _, cookie := range resp.Cookies() {
		if cookie.Name == stateCookieName {
			sawStateCookie = true
		}
	}

	assert.True(t, sawStateCookie, "expected %s cookie to be set", stateCookieName)
}

// TestFullLoginFlow drives the complete authorization-code flow against the
// fake IdP: login -> IdP "authenticates" and redirects to the callback ->
// callback exchanges the code and sets the session -> an authenticated
// request succeeds and GET /api/me reports the right identity -> logout
// clears the session and the next request is rejected again.
func TestFullLoginFlow(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	idp.Subject = "user-42"
	idp.Email = "operator@example.com"
	idp.Name = "Test Operator"

	server, _ := newGatedServerWithRealRedirect(t, idp)
	client := newCookieClient(t)

	// Before login: protected routes are rejected.
	resp, err := client.Get(server.URL + "/api/runs")
	require.NoError(t, err)

	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Drive the flow: GET /auth/login -> fake IdP /authorize -> our
	// /auth/callback -> back into the app. The client follows every
	// redirect and carries cookies across hosts (app + fake IdP), exactly
	// like a real browser.
	resp, err = client.Get(server.URL + "/auth/login?redirect=" + "/runs/abc")
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "login flow did not complete: %s", body)

	var final struct {
		Path     string
		Identity Identity
	}
	require.NoError(t, json.Unmarshal(body, &final))
	assert.Equal(t, "/runs/abc", final.Path, "callback must redirect back to the path login was initiated from")
	assert.Equal(t, "user-42", final.Identity.Subject)
	assert.Equal(t, "operator@example.com", final.Identity.Email)
	assert.Equal(t, "Test Operator", final.Identity.Name)

	// After login: GET /api/me reports the same identity.
	resp, err = client.Get(server.URL + "/api/me")
	require.NoError(t, err)

	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var identity Identity
	require.NoError(t, json.Unmarshal(body, &identity))
	assert.Equal(t, "user-42", identity.Subject)
	assert.Equal(t, "operator@example.com", identity.Email)
	assert.Equal(t, "Test Operator", identity.Name)

	// After login: a previously-protected API route now succeeds.
	resp, err = client.Get(server.URL + "/api/runs")
	require.NoError(t, err)

	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Logout: use a client sharing the same cookie jar but that does NOT
	// follow redirects. Auto-following here would chase /auth/logout's
	// redirect to "/" straight through a fresh, transparent
	// re-authentication against the fake IdP (it has no concept of an
	// existing IdP-side session requiring interaction, unlike a real
	// provider's login prompt) — defeating the point of this check. A real
	// browser tab would stop and show the (empty/login) page too.
	noFollow := &http.Client{Jar: client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err = noFollow.Get(server.URL + "/auth/logout")
	require.NoError(t, err)

	_ = resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "/", resp.Header.Get("Location"))

	// And the next request to a protected route is rejected again.
	resp, err = noFollow.Get(server.URL + "/api/runs")
	require.NoError(t, err)

	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestTamperedSessionCookie_isRejected covers "a tampered session cookie is
// denied the same as no cookie, never a 500".
func TestTamperedSessionCookie_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	value, err := authenticator.encrypt(sessionPurpose, sessionClaims{
		Subject:   "user-1",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	// Swap the last character of the encrypted (base64url) value for a
	// different one, corrupting the GCM authentication tag while staying
	// within the base64url alphabet — a raw byte-level XOR risks producing
	// a byte the HTTP cookie encoder silently strips before the request
	// even reaches the server, which would test transport encoding rather
	// than webauth's own tamper detection.
	tampered := []byte(value)
	if last := tampered[len(tampered)-1]; last == 'A' {
		tampered[len(tampered)-1] = 'B'
	} else {
		tampered[len(tampered)-1] = 'A'
	}

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/runs", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: string(tampered)})

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a tampered cookie must be rejected, not crash the server")
}

// TestExpiredSessionCookie_isRejected covers "an expired session is
// rejected the same as no session at all".
func TestExpiredSessionCookie_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	value, err := authenticator.encrypt(sessionPurpose, sessionClaims{
		Subject:   "user-1",
		ExpiresAt: time.Now().Add(-time.Minute).Unix(), // already expired
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/me", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value})

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestCallback_stateMismatch_isRejected covers CSRF protection: a callback
// whose "state" query parameter does not match the value recorded in the
// state cookie must be rejected, not silently accepted.
func TestCallback_stateMismatch_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	stateCookieValue, err := authenticator.encrypt(statePurpose, stateClaims{
		State:        "expected-state",
		Nonce:        "some-nonce",
		PKCEVerifier: "verifier",
		RedirectPath: "/",
		ExpiresAt:    time.Now().Add(time.Minute).Unix(),
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/auth/callback?code=irrelevant&state=wrong-state", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookieValue})

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestCallback_noStateCookie_redirectsToLogin covers a callback hit cold
// (no state cookie at all, e.g. a stale bookmark or a replayed URL after the
// 10-minute state TTL) failing safely by sending the browser back to start
// a fresh login, rather than erroring.
func TestCallback_noStateCookie_redirectsToLogin(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(server.URL + "/auth/callback?code=irrelevant&state=whatever")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Location"), "/auth/login")
}

// newCookieClient returns an http.Client with a cookie jar, so it behaves
// like a real browser across the app's origin and the fake IdP's origin
// during the multi-hop login redirect chain.
func newCookieClient(t *testing.T) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	return &http.Client{Jar: jar}
}
