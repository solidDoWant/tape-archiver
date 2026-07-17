package webauth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

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
// a protected API route 401s (JSON body) rather than serving anything,
// since a fetch()/XHR caller has no use for an HTML page body; a page
// route is served normally (the SPA, standing in as echoHandler here) even
// without a session, since the SPA itself renders the styled login page
// once it learns (via GET /api/me, still 401-gated) that it has no session
// — see the package doc comment.
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

	t.Run("page route without a session is served, not redirected", func(t *testing.T) {
		resp, err := client.Get(server.URL + "/runs/some-run-id")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode)

		var body struct {
			Path     string
			Identity Identity
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Equal(t, "/runs/some-run-id", body.Path)
		assert.Empty(t, body.Identity.Subject, "no identity should be attached for an unauthenticated request")
	})

	t.Run("root route without a session is served, not redirected", func(t *testing.T) {
		resp, err := client.Get(server.URL + "/")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("login route without a session is served (never gated, even though it is under the general catch-all)", func(t *testing.T) {
		resp, err := client.Get(server.URL + "/login")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

// TestBuildInfo covers GET /api/build-info: ungated (served without a
// session), and reflecting the Authenticator's configured AppVersion/
// FooterHost — including FooterHost being omitted from the JSON body
// entirely when unset, not rendered as a blank string.
func TestBuildInfo(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")

	t.Run("with a footer host configured", func(t *testing.T) {
		cfg := testConfig(t, idp)
		cfg.AppVersion = "v1.2.3"
		cfg.FooterHost = "homelab"

		authenticator, err := New(t.Context(), cfg)
		require.NoError(t, err)

		server := newGatedServer(t, authenticator)

		resp, err := http.Get(server.URL + "/api/build-info")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		assert.JSONEq(t, `{"version":"v1.2.3","footerHost":"homelab"}`, string(body))
	})

	t.Run("with no footer host configured, the field is omitted entirely", func(t *testing.T) {
		cfg := testConfig(t, idp)
		cfg.AppVersion = "v1.2.3"

		authenticator, err := New(t.Context(), cfg)
		require.NoError(t, err)

		server := newGatedServer(t, authenticator)

		resp, err := http.Get(server.URL + "/api/build-info")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		assert.JSONEq(t, `{"version":"v1.2.3"}`, string(body))
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

	// Swap the FIRST character of the encrypted (base64url) value for a
	// different one, corrupting the GCM authentication tag while staying
	// within the base64url alphabet — a raw byte-level XOR risks producing
	// a byte the HTTP cookie encoder silently strips before the request
	// even reaches the server, which would test transport encoding rather
	// than webauth's own tamper detection.
	//
	// It must be the first character, not the last: unpadded base64 (as
	// encoding/base64.RawURLEncoding produces) leaves the final character's
	// low-order bits unused whenever the decoded length isn't a multiple of
	// 3 bytes. Swapping the last character then has a real chance — this
	// was observed flaking in CI, not just a theoretical risk — of only
	// touching those unused bits, decoding back to the exact same bytes and
	// silently failing to tamper anything at all. The first character's
	// bits are always fully used by the first decoded byte, regardless of
	// the value's total length, so this is deterministic.
	tampered := []byte(value)
	if first := tampered[0]; first == 'A' {
		tampered[0] = 'B'
	} else {
		tampered[0] = 'A'
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

// TestCrossSiteMutation_isRejected covers the CSRF defence-in-depth check on
// the mutating API: a state-changing /api/* request a browser labels cross-site
// (via Sec-Fetch-Site or a mismatched Origin) is refused with 403, while
// same-origin requests and non-browser clients (no such headers) pass, and safe
// methods are never blocked.
func TestCrossSiteMutation_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	session, err := authenticator.encrypt(sessionPurpose, sessionClaims{
		Subject:   "user-1",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	tests := []struct {
		name       string
		method     string
		headers    map[string]string
		wantStatus int
	}{
		{
			name:       "same-origin POST via Sec-Fetch-Site is allowed",
			method:     http.MethodPost,
			headers:    map[string]string{"Sec-Fetch-Site": "same-origin"},
			wantStatus: http.StatusOK,
		},
		{
			name:       "cross-site POST via Sec-Fetch-Site is forbidden",
			method:     http.MethodPost,
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusForbidden,
		},
		{
			// A sibling subdomain on the same registrable domain reports
			// same-site and carries its own (mismatched) Origin — the forgery
			// vector the guard names. It must NOT be trusted on same-site alone.
			name:       "same-site POST from a sibling subdomain (mismatched Origin) is forbidden",
			method:     http.MethodPost,
			headers:    map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.example.com"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "cross-origin POST via mismatched Origin is forbidden",
			method:     http.MethodPost,
			headers:    map[string]string{"Origin": "https://evil.example.com"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "same-origin POST via matching Origin is allowed",
			method:     http.MethodPost,
			headers:    map[string]string{"Origin": server.URL},
			wantStatus: http.StatusOK,
		},
		{
			name:       "POST with no browser headers (non-browser client) is allowed",
			method:     http.MethodPost,
			wantStatus: http.StatusOK,
		},
		{
			name:       "cross-site GET (safe method) is unaffected",
			method:     http.MethodGet,
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusOK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(test.method, server.URL+"/api/runs", nil)
			require.NoError(t, err)
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})

			for key, value := range test.headers {
				req.Header.Set(key, value)
			}

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)

			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, test.wantStatus, resp.StatusCode)
		})
	}
}

// TestCallback_stateMismatch_isRejected covers CSRF protection: a callback
// whose "state" query parameter does not match the value recorded in the
// state cookie must be rejected — redirected to the login page with an
// "expired" error, not silently accepted.
func TestCallback_stateMismatch_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	stateCookieValue, err := authenticator.encrypt(statePurpose, stateClaims{
		State:        "expected-state",
		Nonce:        "some-nonce",
		PKCEVerifier: "verifier",
		RedirectPath: "/runs/abc",
		ExpiresAt:    time.Now().Add(time.Minute).Unix(),
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/auth/callback?code=irrelevant&state=wrong-state", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookieValue})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusFound, resp.StatusCode)

	location := resp.Header.Get("Location")
	assert.True(t, strings.HasPrefix(location, "/login?"), "expected a redirect to the login page, got %q", location)
	assert.Contains(t, location, "error=expired")
	assert.Contains(t, location, "redirect=%2Fruns%2Fabc")
}

// TestCallback_noStateCookie_redirectsToLogin covers a callback hit cold
// (no state cookie at all, e.g. a stale bookmark or a replayed URL after the
// 10-minute state TTL) failing safely by sending the browser to the login
// page with an "expired" error (no known redirect target, since the state
// cookie carrying it is exactly what is missing), rather than erroring.
func TestCallback_noStateCookie_redirectsToLogin(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(server.URL + "/auth/callback?code=irrelevant&state=whatever")
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusFound, resp.StatusCode)

	location := resp.Header.Get("Location")
	assert.True(t, strings.HasPrefix(location, "/login?"), "expected a redirect to the login page, got %q", location)
	assert.Contains(t, location, "error=expired")
	assert.NotContains(t, location, "redirect=")
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

// TestCallback_idpError_isRejected covers the IdP declining the login
// (e.g. the user cancels, or the provider rejects the request) reported back
// as an "error" query parameter on the callback — this must redirect to the
// login page with the "denied" error (web/src/LoginPage.tsx's
// error-denied state), not be ignored, and not any other error code.
func TestCallback_idpError_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	stateCookieValue, err := authenticator.encrypt(statePurpose, stateClaims{
		State:        "the-state",
		Nonce:        "the-nonce",
		PKCEVerifier: "verifier",
		RedirectPath: "/runs/abc",
		ExpiresAt:    time.Now().Add(time.Minute).Unix(),
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/auth/callback?error=access_denied&state=the-state", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookieValue})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusFound, resp.StatusCode)

	location := resp.Header.Get("Location")
	assert.True(t, strings.HasPrefix(location, "/login?"), "expected a redirect to the login page, got %q", location)
	assert.Contains(t, location, "error=denied")
	assert.Contains(t, location, "redirect=%2Fruns%2Fabc")
}

// TestCallback_pkceMismatch_isRejected covers the PKCE verifier in the state
// cookie not matching the code_challenge presented at the authorization
// endpoint — the fake IdP's token endpoint enforces RFC 7636 S256 PKCE for
// real, so this exercises the real rejection path, not a stub.
func TestCallback_pkceMismatch_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	code := fakeAuthorize(t, idp, "the-state", "the-nonce", oauth2.S256ChallengeFromVerifier("real-verifier"))

	stateCookieValue, err := authenticator.encrypt(statePurpose, stateClaims{
		State: "the-state",
		Nonce: "the-nonce",
		// Does not match the verifier fakeAuthorize's challenge above was
		// derived from, so the IdP's PKCE check at the token endpoint fails.
		PKCEVerifier: "wrong-verifier",
		RedirectPath: "/",
		ExpiresAt:    time.Now().Add(time.Minute).Unix(),
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/auth/callback?code="+code+"&state=the-state", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookieValue})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusFound, resp.StatusCode)

	location := resp.Header.Get("Location")
	assert.True(t, strings.HasPrefix(location, "/login?"), "expected a redirect to the login page, got %q", location)
	assert.Contains(t, location, "error=expired")
}

// TestCallback_nonceMismatch_isRejected covers the OIDC nonce recorded in
// the state cookie not matching the nonce embedded in the returned,
// correctly-signed ID token — the replay/token-substitution defense the
// nonce exists for.
func TestCallback_nonceMismatch_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	verifier := oauth2.GenerateVerifier()
	code := fakeAuthorize(t, idp, "the-state", "real-nonce", oauth2.S256ChallengeFromVerifier(verifier))

	stateCookieValue, err := authenticator.encrypt(statePurpose, stateClaims{
		State: "the-state",
		// Does not match the nonce fakeAuthorize embedded in the signed ID
		// token above.
		Nonce:        "different-nonce",
		PKCEVerifier: verifier,
		RedirectPath: "/",
		ExpiresAt:    time.Now().Add(time.Minute).Unix(),
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/auth/callback?code="+code+"&state=the-state", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookieValue})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusFound, resp.StatusCode)

	location := resp.Header.Get("Location")
	assert.True(t, strings.HasPrefix(location, "/login?"), "expected a redirect to the login page, got %q", location)
	assert.Contains(t, location, "error=expired")
}

// TestCallback_expiredIDToken_isRejected covers a syntactically valid,
// correctly-signed ID token that ID-token verification (coreos/go-oidc, not
// anything hand-rolled in this package) rejects on its merits — expiry here,
// standing in for the whole class of signature/issuer/audience/expiry
// checks Verify performs. Must redirect to the login page with the
// "expired" error, not succeed and not fail the request outright.
func TestCallback_expiredIDToken_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	idp.IDTokenLifetime = -time.Hour // already expired at issuance

	server, _ := newGatedServerWithRealRedirect(t, idp)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	// Follows the redirect chain through the fake IdP (login -> authorize ->
	// callback) like a real browser, but must NOT follow the final
	// callback -> /login redirect this test is asserting on.
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && strings.HasPrefix(via[len(via)-1].URL.Path, "/auth/callback") {
				return http.ErrUseLastResponse
			}

			return nil
		},
	}

	resp, err := client.Get(server.URL + "/auth/login")
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusFound, resp.StatusCode, "an expired ID token must be rejected by verification: %s", body)

	location := resp.Header.Get("Location")
	assert.True(t, strings.HasPrefix(location, "/login?"), "expected a redirect to the login page, got %q", location)
	assert.Contains(t, location, "error=expired")
}

// fakeAuthorize drives the fake IdP's authorization endpoint directly (not
// through webauth's own /auth/login, which would mint its own
// state/nonce/verifier this test needs to control independently) and returns
// the issued authorization code, for tests that need a real code tied to a
// specific nonce/code_challenge they deliberately mismatch against a forged
// state cookie.
func fakeAuthorize(t *testing.T, idp *testutil.FakeOIDCProvider, state, nonce, codeChallenge string) string {
	t.Helper()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	authorizeURL := idp.Server.URL + "/authorize?" + url.Values{
		"client_id":             {idp.ClientID},
		"redirect_uri":          {"http://app.example.com/auth/callback"},
		"response_type":         {"code"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	resp, err := client.Get(authorizeURL)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusFound, resp.StatusCode)

	location, err := resp.Location()
	require.NoError(t, err)

	code := location.Query().Get("code")
	require.NotEmpty(t, code)

	return code
}

// TestSanitizeRedirectPath covers the post-login open-redirect defense
// directly: a same-origin absolute path is preserved, everything else
// (including a bare "//" and, since browsers normalize "\" to "/" for
// http(s) URLs per the WHATWG URL spec, a "\"-containing path) defaults to
// "/".
func TestSanitizeRedirectPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty defaults to root", in: "", want: "/"},
		{name: "relative without a leading slash defaults to root", in: "runs/abc", want: "/"},
		{name: "a normal same-origin path is preserved", in: "/runs/abc", want: "/runs/abc"},
		{name: "protocol-relative // is rejected", in: "//evil.example", want: "/"},
		{name: "a backslash-prefixed path is rejected", in: "/\\evil.example", want: "/"},
		{name: "a backslash anywhere in the path is rejected", in: "/runs/\\evil.example", want: "/"},
		{name: "the login page itself is rejected (would strand an authed user)", in: "/login", want: "/"},
		{name: "the login page with a trailing slash is rejected (parseRoute treats it as login too)", in: "/login/", want: "/"},
		{name: "the login page with a query is rejected", in: "/login?redirect=%2Flogin", want: "/"},
		{name: "the login page with a trailing slash and a query is rejected", in: "/login/?redirect=%2Flogin", want: "/"},
		{name: "a path merely starting with login is preserved", in: "/login-help", want: "/login-help"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, sanitizeRedirectPath(test.in))
		})
	}
}

// TestCookieSecureFlag_honorsForwardedProto covers the Secure cookie
// attribute reflecting X-Forwarded-Proto, not just r.TLS — cmd/web never
// terminates TLS itself (docs/web-ui-design.md §5's TLS-terminating Ingress
// deployment model), so r.TLS alone would leave Secure permanently false in
// every real deployment.
func TestCookieSecureFlag_honorsForwardedProto(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	t.Run("plain HTTP with no forwarded-proto header is not marked Secure", func(t *testing.T) {
		resp, err := client.Get(server.URL + "/auth/login")
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.False(t, findCookie(t, resp.Cookies(), stateCookieName).Secure)
	})

	t.Run("X-Forwarded-Proto: https marks the cookie Secure", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/auth/login", nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-Proto", "https")

		resp, err := client.Do(req)
		require.NoError(t, err)

		defer func() { _ = resp.Body.Close() }()

		assert.True(t, findCookie(t, resp.Cookies(), stateCookieName).Secure)
	})
}

func findCookie(t *testing.T, cookies []*http.Cookie, name string) *http.Cookie {
	t.Helper()

	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}

	t.Fatalf("cookie %q not found among %d response cookies", name, len(cookies))

	return nil
}
