// Command webdevoidc is a local, standards-compliant OpenID Connect identity
// provider for `make web-dev` (issue #265) — dev tooling only, never built
// into a shipped image or run in CI. It exists because cmd/web (see
// docs/configuration.md's "Web UI environment variables" / "OIDC
// authentication" sections) refuses to start without a real, reachable OIDC
// provider, and there is no such provider available for a one-command local
// run.
//
// It wraps internal/devoidc.NewStandaloneFakeOIDCProvider — the same
// real discovery/JWKS/authorize/token implementation
// pkg/webauth's and cmd/web's own tests already exercise via
// testutil's NewFakeOIDCProvider/NewFakeOIDCProviderOn, extracted
// (issue #265's option (b), package boundary sharpened by issue #267) into
// a form that does not need a testing.TB, which only a Go test binary can
// supply. A standards-compliant fake was chosen over running a
// real IdP (e.g. Dex) in Docker: no new external dependency or image pull,
// and `make web-dev` stays pure Go tooling like mhvtl-up.sh/zpool-up.sh.
// The one real tradeoff: unlike a real IdP's login page, this provider's
// /authorize endpoint has no interactive login form — it immediately
// authenticates the fixed test user configured below and redirects back,
// with nothing to type. See docs/web-ui.md's "Local development" section.
//
// Configuration is entirely via environment variables (all optional, with
// dev-friendly defaults — this is never meant to be pointed at anything but
// a developer's own machine):
//
//   - WEBDEVOIDC_LISTEN_ADDR — address to listen on and advertise as the
//     issuer URL (default "127.0.0.1:9998").
//   - WEBDEVOIDC_CLIENT_ID / WEBDEVOIDC_CLIENT_SECRET — the confidential-client
//     credentials this provider accepts at its token endpoint; must match
//     cmd/web's own OIDC_CLIENT_ID/OIDC_CLIENT_SECRET (scripts/web-dev-up.sh
//     sets both from the same values).
//   - WEBDEVOIDC_SUBJECT / WEBDEVOIDC_EMAIL / WEBDEVOIDC_NAME — the static test
//     user's claims, printed by scripts/web-dev-up.sh as the "login
//     credentials" `make web-dev` reports.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/solidDoWant/tape-archiver/internal/devoidc"
)

const (
	defaultListenAddr   = "127.0.0.1:9998"
	defaultClientID     = "tape-archiver-web-dev"
	defaultClientSecret = "tape-archiver-web-dev-secret" //nolint:gosec // dev-only fixed credential, never used outside a developer's own machine
	defaultSubject      = "dev-operator"
	defaultEmail        = "dev-operator@tape-archiver.local"
	defaultName         = "Dev Operator"

	shutdownTimeout = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "webdevoidc: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	addr := envOr("WEBDEVOIDC_LISTEN_ADDR", defaultListenAddr)
	issuerURL := "http://" + addr

	idp, server, err := devoidc.NewStandaloneFakeOIDCProvider(
		envOr("WEBDEVOIDC_CLIENT_ID", defaultClientID),
		envOr("WEBDEVOIDC_CLIENT_SECRET", defaultClientSecret),
		issuerURL,
	)
	if err != nil {
		return fmt.Errorf("build fake OIDC provider: %w", err)
	}

	idp.Subject = envOr("WEBDEVOIDC_SUBJECT", defaultSubject)
	idp.Email = envOr("WEBDEVOIDC_EMAIL", defaultEmail)
	idp.Name = envOr("WEBDEVOIDC_NAME", defaultName)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)

	go func() {
		serveErr <- server.Serve(listener)
	}()

	fmt.Printf("webdevoidc: listening on %s (issuer %s)\n", addr, issuerURL)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}

		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	return nil
}

// envOr reads the named environment variable, returning fallback when it is
// unset or empty.
func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
