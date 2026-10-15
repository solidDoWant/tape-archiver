// Command webdevoidc is a fake OpenID Connect provider for `make web-dev` (dev
// tooling only), needed because cmd/web refuses to start without a reachable
// OIDC provider. Its /authorize endpoint has no login form: it immediately
// authenticates the fixed test user and redirects back.
//
// Env (all optional): WEBDEVOIDC_LISTEN_ADDR (also the issuer URL),
// WEBDEVOIDC_CLIENT_ID / WEBDEVOIDC_CLIENT_SECRET (must match cmd/web's
// OIDC_CLIENT_ID/OIDC_CLIENT_SECRET), and WEBDEVOIDC_SUBJECT / WEBDEVOIDC_EMAIL
// / WEBDEVOIDC_NAME for the test user's claims.
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

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
