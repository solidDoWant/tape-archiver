// Package health provides an always-on HTTP health server exposing two
// endpoints for orchestrator probes:
//
//   - GET /healthz — liveness. Returns 200 as soon as the server is listening,
//     independent of any downstream dependency. It answers "the process is up
//     and serving"; a failing liveness probe tells Kubernetes to restart the pod.
//   - GET /readyz — readiness. Runs the configured readiness check (typically a
//     Temporal frontend health check) per request and returns 200 when it
//     passes or 503 when it fails. It answers "the process is usefully connected
//     and can do work".
//
// The server is deliberately separate from pkg/metrics: health must stay
// available even when the /metrics endpoint is disabled, and the two answer
// different questions on different ports. When no address is configured the
// Server is a pure no-op: no HTTP server and a safe no-op Shutdown.
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// ReadinessFunc reports whether the worker is ready to serve. A nil return means
// ready (readyz → 200); a non-nil return means not-ready (readyz → 503). It is
// called once per /readyz request and is passed the request context so a slow
// dependency cannot outlive the probe.
type ReadinessFunc func(context.Context) error

// Server owns the health HTTP server. When the health address is empty the
// Server is in no-op mode: no listener is opened and Shutdown is a safe no-op.
type Server struct {
	shutdown func(context.Context) error
}

type config struct {
	healthAddr string
	readiness  ReadinessFunc
}

// Option configures Server construction.
type Option func(*config)

// WithHealthAddr sets the TCP address for the health HTTP server (e.g. ":8080").
// When not supplied, no HTTP server is started.
func WithHealthAddr(addr string) Option {
	return func(c *config) { c.healthAddr = addr }
}

// WithReadinessCheck sets the readiness check consulted by /readyz. When not
// supplied, readiness is treated as always-ready (readyz → 200) — liveness and
// readiness are then equivalent.
func WithReadinessCheck(fn ReadinessFunc) Option {
	return func(c *config) { c.readiness = fn }
}

// New creates a Server. When WithHealthAddr is not supplied, the returned Server
// starts no HTTP server and Shutdown is a safe no-op. When an address is
// supplied, /healthz and /readyz are served on it immediately.
func New(opts ...Option) (*Server, error) {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	if cfg.healthAddr == "" {
		return &Server{shutdown: func(context.Context) error { return nil }}, nil
	}

	listener, err := net.Listen("tcp", cfg.healthAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on health addr %s: %w", cfg.healthAddr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleLiveness)
	mux.HandleFunc("/readyz", handleReadiness(cfg.readiness))

	srv := &http.Server{Handler: mux}

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "health HTTP server error: %v\n", err)
		}
	}()

	return &Server{shutdown: srv.Shutdown}, nil
}

// handleLiveness always reports the process as live. Liveness is intentionally
// independent of any downstream dependency: it must not fail just because
// Temporal (or anything else) is unreachable, or Kubernetes would restart a pod
// that is merely waiting on a dependency to recover.
func handleLiveness(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadiness runs the readiness check per request and maps its result to a
// status code: 200 when ready, 503 when not. A nil check means always-ready.
func handleReadiness(readiness ReadinessFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if readiness != nil {
			if err := readiness(r.Context()); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, "not ready: %v\n", err)

				return
			}
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	}
}

// Shutdown stops the health HTTP server, if one was started. It should be called
// before the process exits.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.shutdown(ctx)
}

// NewFromEnv creates a Server using the HEALTH_ADDR environment variable for the
// listen address, falling back to defaultAddr when HEALTH_ADDR is unset. Pass ""
// as defaultAddr to keep the endpoints disabled when HEALTH_ADDR is unset;
// setting HEALTH_ADDR="" explicitly also disables them. readiness is consulted
// by /readyz on every request (pass nil for always-ready).
//
// The returned shutdown func must be deferred by the caller. It stops the health
// HTTP server with a 10-second deadline and writes any error to stderr.
func NewFromEnv(defaultAddr string, readiness ReadinessFunc) (*Server, func(), error) {
	addr := os.Getenv("HEALTH_ADDR")
	if _, set := os.LookupEnv("HEALTH_ADDR"); !set {
		addr = defaultAddr
	}

	opts := []Option{WithReadinessCheck(readiness)}
	if addr != "" {
		opts = append(opts, WithHealthAddr(addr))
	}

	s, err := New(opts...)
	if err != nil {
		return nil, func() {}, err
	}

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := s.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "health shutdown error: %v\n", err)
		}
	}

	return s, shutdown, nil
}
