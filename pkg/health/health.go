// Package health serves /healthz (liveness, always 200) and /readyz (runs a
// readiness check per request; 503 on failure). It is separate from
// pkg/metrics so probes keep working when /metrics is disabled.
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

// ReadinessFunc returns nil when ready. It is called once per /readyz request.
type ReadinessFunc func(context.Context) error

// Server owns the health HTTP server; with no address it is a no-op.
type Server struct {
	shutdown func(context.Context) error
}

type config struct {
	healthAddr string
	readiness  ReadinessFunc
}

// Option configures Server construction.
type Option func(*config)

// WithHealthAddr sets the listen address; without it no server is started.
func WithHealthAddr(addr string) Option {
	return func(c *config) { c.healthAddr = addr }
}

// WithReadinessCheck sets the /readyz check; without it /readyz is always 200.
func WithReadinessCheck(fn ReadinessFunc) Option {
	return func(c *config) { c.readiness = fn }
}

// New creates a Server, serving immediately when an address is configured.
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

// handleLiveness ignores dependencies so Kubernetes never restarts a pod that
// is merely waiting for one to recover.
func handleLiveness(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

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

// Shutdown stops the health HTTP server, if one was started.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.shutdown(ctx)
}

// NewFromEnv creates a Server listening on HEALTH_ADDR, or defaultAddr when it
// is unset; an empty address disables it. The caller must defer the returned
// shutdown func.
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
