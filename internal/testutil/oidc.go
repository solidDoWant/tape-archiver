// testing.TB-aware wrappers around internal/devoidc's fake OIDC provider, so
// tests exercise real token signing and verification rather than a mock.

package testutil

import (
	"net"
	"net/http/httptest"
	"testing"

	"github.com/solidDoWant/tape-archiver/internal/devoidc"
)

// FakeOIDCProvider is an alias for devoidc.FakeOIDCProvider.
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

// NewFakeOIDCProviderOn is NewFakeOIDCProvider bound to listener (e.g. the kind
// bridge gateway, so both pods and the host browser can reach it). The server
// takes ownership of listener.
func NewFakeOIDCProviderOn(t testing.TB, clientID, clientSecret string, listener net.Listener) *FakeOIDCProvider {
	t.Helper()

	idp, server := newUnstartedFakeOIDCProvider(t, clientID, clientSecret)

	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	return idp
}

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
