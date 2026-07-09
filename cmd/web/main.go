// Command web is the tape-archiver web UI's HTTP server. Today it only serves
// the built React SPA (embedded via go:embed — see assets.go) at "/" and a
// /healthz liveness endpoint; Temporal wiring, the /api/* routes, and OIDC
// auth land in later sub-issues of the web UI epic
// (docs/web-ui-design.md §8). Serving logic lives in pkg/webserver so this
// command stays a thin wrapper: parse configuration, build the handler, run
// an *http.Server, shut it down cleanly on SIGINT/SIGTERM.
//
// Listen address is configured via the WEB_LISTEN_ADDRESS environment
// variable (e.g. ":8080" or "127.0.0.1:8080"), defaulting to ":8080" when
// unset. Log level is configured via LOG_LEVEL (debug, info, warn, error),
// defaulting to info — see pkg/logging.
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

	"github.com/solidDoWant/tape-archiver/pkg/logging"
	"github.com/solidDoWant/tape-archiver/pkg/webserver"
)

// defaultListenAddr is used when WEB_LISTEN_ADDRESS is unset.
const defaultListenAddr = ":8080"

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

// run builds the SPA handler, starts listening, and blocks until ctx is
// cancelled (SIGINT/SIGTERM in main), at which point it shuts the server down
// with a bounded grace period. getenv is injected so tests can drive
// WEB_LISTEN_ADDRESS resolution without mutating the process environment.
// ready, if non-nil, is called once with the bound address as soon as the
// listener is open — tests use it to learn the actual port when listening on
// ":0"; production passes nil.
func run(ctx context.Context, getenv func(string) string, ready func(addr string)) error {
	logging.Setup(getenv("LOG_LEVEL"))

	assets, err := distFS()
	if err != nil {
		return fmt.Errorf("load embedded SPA assets: %w", err)
	}

	handler, err := webserver.NewHandler(assets)
	if err != nil {
		return fmt.Errorf("build web server handler: %w", err)
	}

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
