package webauth

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
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
	"github.com/solidDoWant/tape-archiver/pkg/webserver"
)

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

// echoHandler stands in for the SPA+API handler, echoing the path and the
// attached Identity.
func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		identity, _ := IdentityFromContext(r.Context())

		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":       r.URL.Path,
			"requestURI": r.URL.RequestURI(),
			"identity":   identity,
		})
	})
}

func newGatedServer(t *testing.T, authenticator *Authenticator) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(authenticator.Wrap(echoHandler()))
	t.Cleanup(server.Close)

	return server
}

// newGatedServerWithRealRedirect points RedirectURL at the server itself, for
// tests that actually follow the fake IdP's redirect to the callback.
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

// TestUnauthenticatedRequests_areRejected: API routes 401, page routes still
// serve the SPA (which renders the login page).
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

	var sawStateCookie bool

	for _, cookie := range resp.Cookies() {
		if cookie.Name == stateCookieName {
			sawStateCookie = true
		}
	}

	assert.True(t, sawStateCookie, "expected %s cookie to be set", stateCookieName)
}

// TestFullLoginFlow drives login, callback, authenticated access, and logout
// against the fake IdP.
func TestFullLoginFlow(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	idp.Subject = "user-42"
	idp.Email = "operator@example.com"
	idp.Name = "Test Operator"

	server, _ := newGatedServerWithRealRedirect(t, idp)
	client := newCookieClient(t)

	resp, err := client.Get(server.URL + "/api/runs")
	require.NoError(t, err)

	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

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

	resp, err = client.Get(server.URL + "/api/me")
	require.NoError(t, err)

	body, _ = io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	// Per-operator identity must never be stored by a shared cache.
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	_ = resp.Body.Close()

	var identity Identity
	require.NoError(t, json.Unmarshal(body, &identity))
	assert.Equal(t, "user-42", identity.Subject)
	assert.Equal(t, "operator@example.com", identity.Email)
	assert.Equal(t, "Test Operator", identity.Name)

	resp, err = client.Get(server.URL + "/api/runs")
	require.NoError(t, err)

	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Don't follow redirects on logout: the fake IdP would transparently
	// re-authenticate, defeating the check.
	noFollow := &http.Client{Jar: client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err = noFollow.Get(server.URL + "/auth/logout")
	require.NoError(t, err)

	_ = resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "/", resp.Header.Get("Location"))

	resp, err = noFollow.Get(server.URL + "/api/runs")
	require.NoError(t, err)

	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestLoginRedirectRejectsControlCharacters (issue #383): browsers strip tab,
// CR and LF from URLs, so "/\t/evil.example" would become "//evil.example".
func TestLoginRedirectRejectsControlCharacters(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	server, _ := newGatedServerWithRealRedirect(t, idp)

	tests := []struct {
		name     string
		redirect string
		want     string
	}{
		{name: "tab", redirect: "/%09/evil.example", want: "/"},
		{name: "line feed", redirect: "/%0a/evil.example", want: "/"},
		{name: "carriage return", redirect: "/%0d/evil.example", want: "/"},
		{name: "NUL", redirect: "/%00/evil.example", want: "/"},
		{name: "DEL", redirect: "/%7f/evil.example", want: "/"},
		{name: "control character in the query", redirect: "/runs/abc%3Ftab%3D%09", want: "/"},
		{name: "a normal same-origin path with a query", redirect: "/runs/abc%3Ftab%3Dlogs", want: "/runs/abc?tab=logs"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newCookieClient(t)

			resp, err := client.Get(server.URL + "/auth/login?redirect=" + test.redirect)
			require.NoError(t, err)

			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode, "login flow did not complete: %s", body)

			var final struct {
				RequestURI string `json:"requestURI"`
			}
			require.NoError(t, json.Unmarshal(body, &final))
			assert.Equal(t, test.want, final.RequestURI)
		})
	}
}

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

	// Swap the first base64url character, staying in-alphabet so the cookie
	// encoder doesn't strip it. Not the last: its low bits may be unused
	// padding, which made this flake in CI.
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

func TestIdentityFromRequest(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	cookieFor := func(t *testing.T, claims sessionClaims) *http.Cookie {
		t.Helper()

		value, err := authenticator.encrypt(sessionPurpose, claims)
		require.NoError(t, err)

		return &http.Cookie{Name: sessionCookieName, Value: value}
	}

	t.Run("valid session yields the identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(cookieFor(t, sessionClaims{
			Subject:   "user-1",
			Email:     "op@example.com",
			Name:      "Op Erator",
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}))

		identity, ok := authenticator.IdentityFromRequest(req)
		require.True(t, ok)
		assert.Equal(t, Identity{Subject: "user-1", Email: "op@example.com", Name: "Op Erator"}, identity)
	})

	t.Run("no cookie reports not authenticated", func(t *testing.T) {
		_, ok := authenticator.IdentityFromRequest(httptest.NewRequest(http.MethodGet, "/", nil))
		assert.False(t, ok)
	})

	t.Run("expired session reports not authenticated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(cookieFor(t, sessionClaims{
			Subject:   "user-1",
			ExpiresAt: time.Now().Add(-time.Minute).Unix(),
		}))

		_, ok := authenticator.IdentityFromRequest(req)
		assert.False(t, ok)
	})
}

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
			// Same-site alone must not be trusted.
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

func newCookieClient(t *testing.T) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	return &http.Client{Jar: jar}
}

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

	req, err := http.NewRequest(http.MethodGet,
		server.URL+"/auth/callback?error=access_denied&error_description=User+not+assigned&state=the-state", nil)
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

	loginURL, err := url.Parse(location)
	require.NoError(t, err)
	assert.Equal(t, "User not assigned", loginURL.Query().Get("error_description"))
}

func TestSanitizeIdPErrorDescription(t *testing.T) {
	tests := []struct {
		name        string
		description string
		want        string
	}{
		{name: "empty stays empty", description: "", want: ""},
		{
			name:        "a normal message is preserved",
			description: "The request is otherwise malformed",
			want:        "The request is otherwise malformed",
		},
		{
			name:        "whitespace runs collapse and trim",
			description: "  too\t\tmany\n\nspaces  ",
			want:        "too many spaces",
		},
		{
			name:        "control and bidi/format runes are dropped",
			description: "safe\x00text\u202ehere",
			want:        "safetexthere",
		},
		{
			name:        "over-long input is capped",
			description: strings.Repeat("x", 400),
			want:        strings.Repeat("x", idpErrorDescriptionMaxLen),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, sanitizeIdPErrorDescription(test.description))
		})
	}
}

// TestCallback_idpError_isLoggedToAccessLog covers the prod "authenticated but
// not authorized" report: the IdP's error is the only diagnostic and must
// reach the access log at warn despite the 302.
func TestCallback_idpError_isLoggedToAccessLog(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	server := httptest.NewServer(webserver.AccessLog(logger, nil, authenticator.Wrap(echoHandler())))
	t.Cleanup(server.Close)

	stateCookieValue, err := authenticator.encrypt(statePurpose, stateClaims{
		State:        "the-state",
		Nonce:        "the-nonce",
		PKCEVerifier: "verifier",
		RedirectPath: "/runs/abc",
		ExpiresAt:    time.Now().Add(time.Minute).Unix(),
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet,
		server.URL+"/auth/callback?error=access_denied&error_description=User+not+assigned+to+application&state=the-state", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookieValue})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusFound, resp.StatusCode)

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &record))

	assert.Equal(t, "/auth/callback", record["path"])
	assert.Equal(t, "WARN", record["level"], "a login failure must stay visible at LOG_LEVEL=warn")
	assert.Equal(t, "identity provider returned an error", record["auth_error"])
	assert.Equal(t, "access_denied", record["idp_error"])
	assert.Equal(t, "User not assigned to application", record["idp_error_description"])
	assert.NotContains(t, buf.String(), "the-state", "the query string (state) must never be logged")
}

// TestCallback_pkceMismatch_isRejected relies on the fake IdP enforcing S256
// PKCE for real at its token endpoint.
func TestCallback_pkceMismatch_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	code := fakeAuthorize(t, idp, "the-state", "the-nonce", oauth2.S256ChallengeFromVerifier("real-verifier"))

	stateCookieValue, err := authenticator.encrypt(statePurpose, stateClaims{
		State: "the-state",
		Nonce: "the-nonce",
		// Mismatches the challenge passed to fakeAuthorize.
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

func TestCallback_nonceMismatch_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	authenticator, err := New(t.Context(), testConfig(t, idp))
	require.NoError(t, err)

	server := newGatedServer(t, authenticator)

	verifier := oauth2.GenerateVerifier()
	code := fakeAuthorize(t, idp, "the-state", "real-nonce", oauth2.S256ChallengeFromVerifier(verifier))

	stateCookieValue, err := authenticator.encrypt(statePurpose, stateClaims{
		State: "the-state",
		// Mismatches the nonce in the signed ID token.
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

func TestCallback_expiredIDToken_isRejected(t *testing.T) {
	idp := testutil.NewFakeOIDCProvider(t, "client-1", "secret-1")
	idp.IDTokenLifetime = -time.Hour // already expired at issuance

	server, _ := newGatedServerWithRealRedirect(t, idp)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	// Stop at the callback's redirect, which is what's asserted on.
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

// fakeAuthorize gets a real code from the fake IdP for a caller-chosen
// nonce/challenge, bypassing /auth/login.
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
