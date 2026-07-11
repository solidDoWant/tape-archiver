// This file provides testing.TB-aware helpers on top of internal/devoidc's
// FakeOIDCProvider: a minimal, in-process, standards-compliant OpenID
// Connect identity provider used to exercise pkg/webauth's
// authorization-code flow end to end, in both pkg/webauth's own unit tests
// and cmd/web's integration tests. No real OIDC identity provider is
// available in this sandbox, and pkg/webauth must work against any
// compliant provider rather than a specific one (docs/web-ui-design.md
// §4/§6), so tests drive this fake instead of a real IdP or a mocked-out
// webauth: it serves real discovery, JWKS, an authorization endpoint, and a
// token endpoint, signing ID tokens with a throwaway RSA key via the same
// go-jose/v4 library coreos/go-oidc uses internally to verify them — real
// signature/issuer/audience/nonce verification runs on every test that uses
// it, nothing about that verification is mocked away.
//
// The provider implementation itself — discovery/JWKS/authorize/token,
// PKCE, ID token signing — lives in internal/devoidc, which has no
// dependency on `testing` and is safe to import from non-test binaries (see
// cmd/webdevoidc, `make web-dev`'s local fake OIDC provider, which imports
// internal/devoidc directly rather than this package).

package testutil

import (
	"net"
	"net/http/httptest"
	"testing"

	"github.com/solidDoWant/tape-archiver/internal/devoidc"
)

// FakeOIDCProvider is an alias for devoidc.FakeOIDCProvider, kept here so
// existing call sites that reference testutil.FakeOIDCProvider by name
// (pkg/webauth's tests, cmd/web's integration test, e2e/web_test.go)
// continue to compile against this package unchanged.
type FakeOIDCProvider = devoidc.FakeOIDCProvider

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

// newUnstartedFakeOIDCProvider builds the fake provider and its mux via
// internal/devoidc.NewUnstarted, wiring the result into an unstarted
// httptest.Server so both NewFakeOIDCProvider (default loopback listener) and
// NewFakeOIDCProviderOn (caller-supplied listener) share one construction
// path.
func newUnstartedFakeOIDCProvider(t testing.TB, clientID, clientSecret string) (*FakeOIDCProvider, *httptest.Server) {
	t.Helper()

	idp, mux, err := devoidc.NewUnstarted(clientID, clientSecret)
	if err != nil {
		t.Fatalf("testutil: %v", err)
	}

	server := httptest.NewUnstartedServer(mux)
	idp.Server = server

	return idp, server
}
