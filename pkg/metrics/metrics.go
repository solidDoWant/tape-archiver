// Package metrics provides a Prometheus registry with an optional /metrics
// server; with no address configured it is a no-op.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// DefaultScrapeWaitTimeout is the default upper bound for Provider.WaitForScrape.
const DefaultScrapeWaitTimeout = 60 * time.Second

// Provider exposes a Prometheus registry and its /metrics endpoint.
type Provider struct {
	promRegistry      *prometheus.Registry
	shutdown          func(context.Context) error
	scrapeNotify      chan struct{}
	scrapeWaitTimeout time.Duration
}

type config struct {
	metricsAddr       string
	scrapeWaitTimeout *time.Duration
}

// Option configures Provider construction.
type Option func(*config)

// WithMetricsAddr sets the listen address; without it no server is started.
func WithMetricsAddr(addr string) Option {
	return func(c *config) { c.metricsAddr = addr }
}

// WithScrapeWaitTimeout bounds Provider.WaitForScrape; a non-positive value
// disables the wait (distinct from omitting the option).
func WithScrapeWaitTimeout(d time.Duration) Option {
	return func(c *config) { c.scrapeWaitTimeout = &d }
}

// New creates a Provider, serving /metrics immediately when an address is set.
func New(opts ...Option) (*Provider, error) {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	scrapeWaitTimeout := DefaultScrapeWaitTimeout
	if cfg.scrapeWaitTimeout != nil {
		scrapeWaitTimeout = *cfg.scrapeWaitTimeout
	}

	if cfg.metricsAddr == "" {
		return &Provider{
			shutdown:          func(context.Context) error { return nil },
			scrapeWaitTimeout: scrapeWaitTimeout,
		}, nil
	}

	promRegistry := prometheus.NewRegistry()

	if err := promRegistry.Register(collectors.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("register Go runtime collector: %w", err)
	}

	listener, err := net.Listen("tcp", cfg.metricsAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on metrics addr %s: %w", cfg.metricsAddr, err)
	}

	// Size 1 suffices: WaitForScrape drains stale ticks before blocking.
	scrapeNotify := make(chan struct{}, 1)
	promHandler := promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{})

	mux := http.NewServeMux()
	mux.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		promHandler.ServeHTTP(w, r)

		select {
		case scrapeNotify <- struct{}{}:
		default:
		}
	}))
	srv := &http.Server{Handler: mux}

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "metrics HTTP server error: %v\n", err)
		}
	}()

	return &Provider{
		promRegistry:      promRegistry,
		shutdown:          srv.Shutdown,
		scrapeNotify:      scrapeNotify,
		scrapeWaitTimeout: scrapeWaitTimeout,
	}, nil
}

// PrometheusRegisterer returns the registry, or an untyped nil when metrics are
// off (avoiding the typed-nil interface gotcha).
func (p *Provider) PrometheusRegisterer() prometheus.Registerer {
	if p.promRegistry == nil {
		return nil
	}

	return p.promRegistry
}

// Shutdown stops the metrics HTTP server, if one was started.
func (p *Provider) Shutdown(ctx context.Context) error {
	return p.shutdown(ctx)
}

// WaitForScrape blocks until a /metrics scrape starts after this call, the
// timeout elapses, or ctx ends, so Prometheus can collect final metrics before
// shutdown. Pass a non-cancelled ctx: a SIGTERM-cancelled one defeats the wait.
func (p *Provider) WaitForScrape(ctx context.Context) error {
	if p.scrapeNotify == nil {
		return nil
	}

	if p.scrapeWaitTimeout <= 0 {
		return nil
	}

	// Drop a tick from a scrape that predates this call.
	select {
	case <-p.scrapeNotify:
	default:
	}

	waitCtx, cancel := context.WithTimeout(ctx, p.scrapeWaitTimeout)
	defer cancel()

	select {
	case <-p.scrapeNotify:
		return nil
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
}

// NewFromEnv creates a Provider listening on METRICS_ADDR, or defaultAddr when
// it is unset; an empty address disables it. METRICS_SCRAPE_WAIT_TIMEOUT
// overrides the caller's defaults. The caller must defer the returned shutdown.
func NewFromEnv(defaultAddr string, defaults ...Option) (*Provider, func(), error) {
	// Explicitly empty disables the endpoint; unset falls back to defaultAddr.
	addr, set := os.LookupEnv("METRICS_ADDR")
	if !set {
		addr = defaultAddr
	}

	// Env-derived options come last so they take precedence.
	opts := append([]Option{}, defaults...)
	if addr != "" {
		opts = append(opts, WithMetricsAddr(addr))
	}

	if raw := os.Getenv("METRICS_SCRAPE_WAIT_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, func() {}, fmt.Errorf("METRICS_SCRAPE_WAIT_TIMEOUT must be a valid duration (got %q): %w", raw, err)
		}

		opts = append(opts, WithScrapeWaitTimeout(d))
	}

	p, err := New(opts...)
	if err != nil {
		return nil, func() {}, err
	}

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := p.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "metrics shutdown error: %v\n", err)
		}
	}

	return p, shutdown, nil
}
