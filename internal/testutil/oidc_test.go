package testutil_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/tape-archiver/internal/testutil"
)

// TestNewStandaloneFakeOIDCProvider_realAuthCodeFlow exercises
// NewStandaloneFakeOIDCProvider — the testing.TB-free constructor cmd/webdevoidc
// uses for `make web-dev` — end to end against a real listener: discovery,
// JWKS, a real PKCE authorize/token round trip, and ID token signature/claim
// verification. This is the same real-provider behavior
// NewFakeOIDCProvider/NewFakeOIDCProviderOn already get exercised through by
// pkg/webauth's tests; this test instead drives the standalone constructor
// directly (no testing.TB involved in the provider under test) so a future
// change to the shared buildFakeOIDCProvider construction path cannot silently
// break the standalone path while the two testing.TB-based ones stay green.
func TestNewStandaloneFakeOIDCProvider_realAuthCodeFlow(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")

	issuerURL := "http://" + listener.Addr().String()

	idp, server, err := testutil.NewStandaloneFakeOIDCProvider("dev-client", "dev-secret", issuerURL)
	require.NoError(t, err, "NewStandaloneFakeOIDCProvider")

	idp.Subject = "dev-operator"
	idp.Email = "dev@example.com"
	idp.Name = "Dev Operator"

	go func() { _ = server.Serve(listener) }()

	t.Cleanup(func() {
		// t.Context() is already canceled by the time Cleanup funcs run, so a
		// fresh deadline needs context.WithoutCancel to strip that
		// cancellation first — same pattern e2e/harness_test.go's
		// terminateOnCleanup already uses for the same reason.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()

		_ = server.Shutdown(ctx)
	})

	// Discovery must advertise the exact issuerURL passed in, not something
	// derived from the listener (there is no httptest.Server to derive it
	// from in the standalone path).
	discoveryResp, err := http.Get(issuerURL + "/.well-known/openid-configuration")
	require.NoError(t, err, "GET discovery")

	defer func() { _ = discoveryResp.Body.Close() }()

	require.Equal(t, http.StatusOK, discoveryResp.StatusCode)

	var discovery struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		JWKSURI               string `json:"jwks_uri"`
	}
	require.NoError(t, json.NewDecoder(discoveryResp.Body).Decode(&discovery))
	assert.Equal(t, issuerURL, discovery.Issuer)
	assert.Equal(t, issuerURL+"/authorize", discovery.AuthorizationEndpoint)
	assert.Equal(t, issuerURL+"/token", discovery.TokenEndpoint)
	assert.Equal(t, issuerURL+"/keys", discovery.JWKSURI)

	// Real PKCE (S256): generate a verifier/challenge pair the same way a real
	// OIDC client library would.
	verifier := "a-fixed-test-verifier-that-is-long-enough-for-pkce-1234567890"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	redirectURI := "http://client.example.com/callback"

	httpClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	authorizeURL := issuerURL + "/authorize?" + url.Values{
		"client_id":             {"dev-client"},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"state":                 {"the-state"},
		"nonce":                 {"the-nonce"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	authorizeResp, err := httpClient.Get(authorizeURL)
	require.NoError(t, err, "GET authorize")

	defer func() { _ = authorizeResp.Body.Close() }()

	require.Equal(t, http.StatusFound, authorizeResp.StatusCode)

	location, err := authorizeResp.Location()
	require.NoError(t, err, "authorize redirect Location")
	assert.Equal(t, "the-state", location.Query().Get("state"))

	code := location.Query().Get("code")
	require.NotEmpty(t, code, "authorize must hand back a code")

	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {"dev-client"},
		"client_secret": {"dev-secret"},
		"code_verifier": {verifier},
	}

	tokenResp, err := http.PostForm(issuerURL+"/token", tokenForm)
	require.NoError(t, err, "POST token")

	defer func() { _ = tokenResp.Body.Close() }()

	require.Equal(t, http.StatusOK, tokenResp.StatusCode)

	var tokenBody struct {
		IDToken string `json:"id_token"`
	}
	require.NoError(t, json.NewDecoder(tokenResp.Body).Decode(&tokenBody))
	require.NotEmpty(t, tokenBody.IDToken)

	// Verify the ID token's signature against the provider's own JWKS (real
	// signature verification, not a mocked-out check) and assert its claims.
	jwksResp, err := http.Get(issuerURL + "/keys")
	require.NoError(t, err, "GET jwks")

	defer func() { _ = jwksResp.Body.Close() }()

	var jwks jose.JSONWebKeySet
	require.NoError(t, json.NewDecoder(jwksResp.Body).Decode(&jwks))
	require.Len(t, jwks.Keys, 1)

	parsed, err := jwt.ParseSigned(tokenBody.IDToken, []jose.SignatureAlgorithm{jose.RS256})
	require.NoError(t, err, "parse ID token")

	var claims struct {
		jwt.Claims

		Nonce string `json:"nonce"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	require.NoError(t, parsed.Claims(jwks.Keys[0].Key, &claims), "verify ID token signature")

	assert.Equal(t, issuerURL, claims.Issuer)
	assert.Equal(t, "dev-operator", claims.Subject)
	assert.Contains(t, claims.Audience, "dev-client")
	assert.Equal(t, "the-nonce", claims.Nonce)
	assert.Equal(t, "dev@example.com", claims.Email)
	assert.Equal(t, "Dev Operator", claims.Name)
}
