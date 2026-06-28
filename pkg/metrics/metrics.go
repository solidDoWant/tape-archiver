// Package metrics provides a registry-based Prometheus provider with an
// optional /metrics HTTP server. Downstream packages register their collectors
// against the registry returned by Provider.PrometheusRegisterer; the same
// registry backs the /metrics endpoint. When no metrics address is configured
// the Provider is a pure no-op: nil registry, no HTTP server, and safe no-op
// Shutdown/WaitForScrape.
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

// DefaultScrapeWaitTimeout is the default upper bound for Provider.WaitForScrape
// when no explicit timeout is configured via WithScrapeWaitTimeout or
// METRICS_SCRAPE_WAIT_TIMEOUT.
const DefaultScrapeWaitTimeout = 60 * time.Second

// Provider exposes a Prometheus /metrics endpoint backed by a
// prometheus.Registry. Application code registers collectors against the
// registry via PrometheusRegisterer; the same registry serves /metrics.
// When the metrics address is empty, PrometheusRegisterer returns nil so
// downstream consumers can detect "metrics off" and short-circuit to a noop,
// the HTTP server is not started, and Shutdown + WaitForScrape are safe
// no-ops.
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

// WithMetricsAddr sets the TCP address for the Prometheus /metrics HTTP server
// (e.g. ":9090"). When not supplied, no HTTP server is started.
func WithMetricsAddr(addr string) Option {
	return func(c *config) { c.metricsAddr = addr }
}

// WithScrapeWaitTimeout sets the upper bound for Provider.WaitForScrape.
// When this option is not supplied at all, DefaultScrapeWaitTimeout is used.
// A non-positive value (e.g. 0) disables the gate: WaitForScrape returns nil
// immediately rather than blocking. Distinguishing "not provided" from
// "explicitly zero" lets operators short-circuit the wait via
// METRICS_SCRAPE_WAIT_TIMEOUT=0s without falling back to the default.
func WithScrapeWaitTimeout(d time.Duration) Option {
	return func(c *config) { c.scrapeWaitTimeout = &d }
}

// New creates a Provider. When WithMetricsAddr is not supplied, the returned
// Provider has a nil registry, starts no HTTP server, and Shutdown +
// WaitForScrape are safe no-ops. When an address is supplied, the Go runtime
// collector is registered against the registry so the standard runtime metrics
// appear on scrape even before any application collectors are added.
func New(opts ...Option) (*Provider, error) {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	// Resolve the configured timeout. nil → not provided → use the default.
	// A non-nil value (including zero or negative) is honored as-is;
	// WaitForScrape treats a non-positive timeout as "gate disabled" and
	// returns nil immediately.
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

	// A fresh registry is empty; register the Go runtime collector so standard
	// runtime metrics (go_goroutines, go_memstats_*, ...) are present on scrape.
	if err := promRegistry.Register(collectors.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("register Go runtime collector: %w", err)
	}

	listener, err := net.Listen("tcp", cfg.metricsAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on metrics addr %s: %w", cfg.metricsAddr, err)
	}

	// Buffered notify channel signals that a /metrics scrape has been served.
	// Buffer size 1 is enough: WaitForScrape drains any pending tick before
	// it blocks, so only fresh post-drain scrapes can satisfy the wait.
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

// PrometheusRegisterer returns the Prometheus registry collectors should be
// registered against, or nil when the Provider is in no-op mode (no metrics
// address). The same registry backs the /metrics endpoint when one is enabled.
// The nil return lets downstream consumers detect "metrics off" and
// short-circuit.
//
// The return type is the prometheus.Registerer interface; a nil *Registry
// is mapped to a nil interface so callers can use a plain `if reg == nil`
// check without falling into the typed-nil interface gotcha.
func (p *Provider) PrometheusRegisterer() prometheus.Registerer {
	if p.promRegistry == nil {
		return nil
	}

	return p.promRegistry
}

// Shutdown stops the metrics HTTP server, if one was started.
// It should be called before the process exits.
func (p *Provider) Shutdown(ctx context.Context) error {
	return p.shutdown(ctx)
}

// WaitForScrape blocks until the /metrics HTTP handler serves a request after
// this call begins, the configured scrape-wait timeout elapses, or ctx is
// cancelled (whichever happens first). It returns nil immediately when no
// Prometheus HTTP server is configured or when the configured timeout is
// non-positive (gate explicitly disabled).
//
// Intended use: invoke after process drain but before exporter shutdown so
// Prometheus has the opportunity to collect a final scrape covering
// end-of-lifecycle metrics. The HTTP server is left running for the duration
// of the wait. Callers should pass a non-cancelled parent context (typically
// context.Background()) — passing a SIGTERM-cancelled context defeats the gate.
func (p *Provider) WaitForScrape(ctx context.Context) error {
	if p.scrapeNotify == nil {
		return nil
	}

	if p.scrapeWaitTimeout <= 0 {
		return nil
	}

	// Drain any tick buffered by a scrape that arrived before this call so
	// only a fresh post-drain scrape can satisfy the wait.
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

// NewFromEnv creates a Provider using standard environment variables.
// METRICS_ADDR sets the Prometheus /metrics HTTP listen address; when unset,
// defaultAddr is used instead. Pass "" as defaultAddr to keep the endpoint
// disabled when METRICS_ADDR is unset. METRICS_SCRAPE_WAIT_TIMEOUT (a Go
// duration string) bounds Provider.WaitForScrape.
//
// The returned shutdown func must be deferred by the caller. It stops the
// metrics HTTP server with a 10-second deadline and writes any error to
// stderr.
func NewFromEnv(defaultAddr string) (*Provider, func(), error) {
	addr := os.Getenv("METRICS_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	var opts []Option
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
