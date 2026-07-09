// Command web is the tape-archiver web UI's HTTP server: it serves the built
// React SPA (embedded via go:embed — see assets.go) at "/" and the JSON API
// (pkg/runsapi) under "/api/*", backed by a real Temporal client
// (pkg/temporalclient — same TEMPORAL_* envconfig as cmd/worker/cmd/tapectl),
// gated behind OIDC authorization-code-flow authentication (pkg/webauth —
// docs/web-ui-design.md §4, §6). Serving logic lives in
// pkg/webserver/pkg/runsapi/pkg/webauth so this command stays a thin
// wrapper: parse configuration, build the Temporal client and handler, run
// health/metrics/main HTTP servers, shut them all down cleanly on
// SIGINT/SIGTERM.
//
// Main listen address is configured via the WEB_LISTEN_ADDRESS environment
// variable (e.g. ":8080" or "127.0.0.1:8080"), defaulting to ":8080" when
// unset. Liveness/readiness (/healthz, /readyz — pkg/health) are served on
// their own address, HEALTH_ADDR, defaulting to ":8081" — deliberately
// distinct from WEB_LISTEN_ADDRESS's own ":8080" default, since (unlike
// cmd/worker, which serves nothing else on its health port) cmd/web's main
// port already answers real traffic. Prometheus metrics (/metrics —
// pkg/metrics) are served on METRICS_ADDR, defaulting to ":9090" — the same
// default cmd/worker uses, which is fine since cmd/web runs as its own
// Kubernetes Deployment/pod (docs/web-ui-design.md §5), not sharing a pod
// with the worker. Log level is configured via LOG_LEVEL (debug, info, warn,
// error), defaulting to info — see pkg/logging.
//
// OIDC authentication (pkg/webauth) is required, not optional: every page
// and API route is gated behind a valid session
// (docs/web-ui-design.md §2 — "Auth: ... all API and page routes gated"), so
// cmd/web refuses to start without a complete OIDC configuration, the same
// way it refuses to start without TEMPORAL_ADDRESS. It is provider-agnostic
// (discovered purely via OIDC discovery — no IdP-specific code) and
// configured via:
//
//   - OIDC_ISSUER_URL — the identity provider's issuer URL, used for
//     discovery.
//   - OIDC_CLIENT_ID / OIDC_CLIENT_SECRET — this app's confidential-client
//     credentials at that provider.
//   - OIDC_REDIRECT_URL — this app's callback URL as registered with the
//     provider (e.g. "https://tape-archiver.example.com/auth/callback").
//   - WEB_SESSION_KEY — a base64-encoded 32-byte AES-256 key (e.g. the
//     output of `openssl rand -base64 32`) encrypting the session and
//     login-state cookies. The service holds no server-side session store
//     (docs/web-ui-design.md §3), so losing/rotating this key just signs
//     everyone out; nothing else depends on it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/pkg/health"
	"github.com/solidDoWant/tape-archiver/pkg/logging"
	"github.com/solidDoWant/tape-archiver/pkg/metrics"
	"github.com/solidDoWant/tape-archiver/pkg/runsapi"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/pkg/webauth"
	"github.com/solidDoWant/tape-archiver/pkg/webserver"
)

// defaultListenAddr is used when WEB_LISTEN_ADDRESS is unset.
const defaultListenAddr = ":8080"

// defaultHealthAddr is the /healthz + /readyz listen address used when
// HEALTH_ADDR is not set. See the package doc comment for why this differs
// from cmd/worker's :8080 default.
const defaultHealthAddr = ":8081"

// defaultMetricsAddr is the /metrics listen address used when METRICS_ADDR is
// not set, matching cmd/worker's default.
const defaultMetricsAddr = ":9090"

// Timeouts guard the server against slow/stalled clients (e.g. Slowloris-style
// header trickling) holding a connection open indefinitely.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Getenv, nil); err != nil {
		fmt.Fprintf(os.Stderr, "web: %v\n", err)
		os.Exit(1)
	}
}

// run sets up metrics, the Temporal client, health, and the SPA+API handler,
// starts listening, and blocks until ctx is cancelled (SIGINT/SIGTERM in
// main), at which point it shuts everything down with a bounded grace
// period, mirroring cmd/worker/main.go's setup/shutdown ordering. getenv is
// injected so tests can drive WEB_LISTEN_ADDRESS resolution without mutating
// the process environment; HEALTH_ADDR/METRICS_ADDR/TEMPORAL_* are read
// directly from the process environment by pkg/health/pkg/metrics/
// pkg/temporalclient, same as cmd/worker. ready, if non-nil, is called once
// with the main server's bound address as soon as its listener is open —
// tests use it to learn the actual port when listening on ":0"; production
// passes nil.
func run(ctx context.Context, getenv func(string) string, ready func(addr string)) error {
	logging.Setup(getenv("LOG_LEVEL"))

	assets, err := distFS()
	if err != nil {
		return fmt.Errorf("load embedded SPA assets: %w", err)
	}

	metricsProvider, metricsShutdown, err := metrics.NewFromEnv(defaultMetricsAddr)
	if err != nil {
		return fmt.Errorf("set up metrics: %w", err)
	}
	defer metricsShutdown()

	// SDK metrics register against the same Prometheus registry as
	// application metrics so they share one /metrics endpoint. The registerer
	// is nil when the metrics endpoint is disabled, which temporalclient.New
	// treats as "SDK metrics off".
	temporalClient, temporalShutdown, err := temporalclient.New(ctx, metricsProvider.PrometheusRegisterer())
	if err != nil {
		// A shutdown signal during the connect/health-check window cancels
		// ctx and surfaces here; that is an orderly stop, not a startup
		// failure.
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer temporalShutdown()

	// The health server exposes liveness (/healthz, always OK once serving)
	// and readiness (/readyz, gated on live Temporal connectivity). Readiness
	// re-checks Temporal per probe so a web server that loses its Temporal
	// connection after startup reports not-ready without the process
	// exiting. It is a dedicated always-on port, independent of the main
	// listen address and /metrics.
	_, healthShutdown, err := health.NewFromEnv(defaultHealthAddr, func(ctx context.Context) error {
		_, err := temporalClient.CheckHealth(ctx, &client.CheckHealthRequest{})

		return err
	})
	if err != nil {
		return fmt.Errorf("set up health server: %w", err)
	}
	// healthShutdown is called explicitly right after the main listener stops
	// accepting connections (not deferred to the end of run()): /readyz only
	// reflects Temporal connectivity, so if it stayed up through the
	// metrics-scrape grace period below, it would keep reporting 200 for that
	// whole window after the main port had already stopped accepting new
	// connections — during which a Kubernetes Service would keep routing new
	// traffic at a pod no longer serving it. Falling through this function
	// without an explicit shutdown call (e.g. an early return before it) still
	// leaks the health server; that only happens on startup failures where the
	// process is about to exit anyway.
	healthShutdownPending := true

	defer func() {
		if healthShutdownPending {
			healthShutdown()
		}
	}()

	oidcConfig, err := oidcConfigFromEnv(getenv)
	if err != nil {
		return fmt.Errorf("configure OIDC: %w", err)
	}

	authenticator, err := webauth.New(ctx, oidcConfig)
	if err != nil {
		// As with temporalclient.New above, a shutdown signal cancelling ctx
		// during OIDC discovery is an orderly stop, not a startup failure.
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("set up OIDC authenticator: %w", err)
	}

	handler, err := webserver.NewHandler(assets, runsapi.New(temporalClient))
	if err != nil {
		return fmt.Errorf("build web server handler: %w", err)
	}

	// authenticator.Wrap gates the SPA + /api/* handler above behind a valid
	// session, and adds the /auth/login, /auth/callback, /auth/logout, and
	// /api/me routes it needs to establish one — see pkg/webauth's package
	// doc comment for the full route/gating contract.
	handler = authenticator.Wrap(handler)

	addr := listenAddr(getenv)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	if ready != nil {
		ready(listener.Addr().String())
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	serveErr := make(chan error, 1)

	go func() {
		serveErr <- srv.Serve(listener)
	}()

	slog.Info("web: listening", "addr", listener.Addr().String())

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}

		return nil
	}

	slog.Info("web: shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// The main listener no longer accepts connections as of the line above;
	// stop reporting ready now rather than waiting for the deferred shutdown
	// at the end of run() — see the comment where healthShutdown is set up.
	healthShutdown()

	healthShutdownPending = false

	// Give Prometheus a bounded window to collect a final scrape of
	// end-of-lifecycle metrics before the deferred metricsShutdown() closes
	// the /metrics server, mirroring cmd/worker/main.go. A non-cancelled
	// context is used deliberately: the SIGTERM-driven shutdown that got us
	// here also cancelled ctx, and passing that cancelled context would
	// defeat the wait. A timeout here is expected and benign, so it is logged
	// at debug rather than failing. WaitForScrape is a no-op when metrics are
	// disabled or the configured timeout is non-positive.
	if err := metricsProvider.WaitForScrape(context.Background()); err != nil {
		slog.Debug("final metrics scrape wait ended without a scrape", "error", err)
	}

	return nil
}

// listenAddr resolves the TCP address to listen on: WEB_LISTEN_ADDRESS when
// set to a non-empty value, defaultListenAddr otherwise.
func listenAddr(getenv func(string) string) string {
	if addr := getenv("WEB_LISTEN_ADDRESS"); addr != "" {
		return addr
	}

	return defaultListenAddr
}

// oidcConfigFromEnv builds a webauth.Config from OIDC_ISSUER_URL /
// OIDC_CLIENT_ID / OIDC_CLIENT_SECRET / OIDC_REDIRECT_URL / WEB_SESSION_KEY.
// All five are required — see the package doc comment for why OIDC
// configuration is mandatory rather than optional. webauth.New performs its
// own presence/format validation on the result, so this function does not
// duplicate it beyond decoding the session key (which webauth.Config takes
// as raw key bytes, not the env var's base64 string).
func oidcConfigFromEnv(getenv func(string) string) (webauth.Config, error) {
	sessionKey, err := webauth.ParseSessionKey(getenv("WEB_SESSION_KEY"))
	if err != nil {
		return webauth.Config{}, err
	}

	return webauth.Config{
		IssuerURL:    getenv("OIDC_ISSUER_URL"),
		ClientID:     getenv("OIDC_CLIENT_ID"),
		ClientSecret: getenv("OIDC_CLIENT_SECRET"),
		RedirectURL:  getenv("OIDC_REDIRECT_URL"),
		SessionKey:   sessionKey,
	}, nil
}
