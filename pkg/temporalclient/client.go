// Package temporalclient builds a Temporal SDK client configured via the
// envconfig package, with two extensions on top:
//
//   - file:// API keys: when the resolved API key has the form
//     "file:///absolute/path", the path is read on every RPC via
//     [client.NewAPIKeyDynamicCredentials], so external rotators that update
//     the file in place are picked up without a process restart.
//   - Startup health check: [New] verifies the gRPC connection to the
//     frontend before returning. The underlying [client.Dial] is lazy and
//     would otherwise return a healthy-looking client even when the server
//     is unreachable.
package temporalclient

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"
)

// healthCheckTimeout is how long New waits for the startup CheckHealth probe.
const healthCheckTimeout = 10 * time.Second

// New loads Temporal client options via envconfig, expands a file:// API key
// into dynamic credentials when present, verifies the gRPC connection via
// CheckHealth, and bridges Temporal SDK metrics onto reg so they appear on
// the same /metrics endpoint as application metrics. Pass nil for reg to
// disable SDK metrics entirely.
//
// The returned shutdown function must be deferred by the caller. It flushes
// pending SDK metric samples, stops the tally reporter goroutine, and closes
// the Temporal client. It is safe to call more than once. When New returns
// a non-nil error the shutdown is a no-op so callers can `defer shutdown()`
// unconditionally.
func New(ctx context.Context, reg prometheus.Registerer) (client.Client, func(), error) {
	metricsHandler, metricsCloser := newMetricsHandler(reg)

	opts, err := buildOptions(metricsHandler)
	if err != nil {
		_ = metricsCloser.Close()

		return nil, noopShutdown, err
	}

	c, err := client.Dial(opts)
	if err != nil {
		_ = metricsCloser.Close()

		return nil, noopShutdown, fmt.Errorf("dial Temporal: %w", err)
	}

	healthCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	if _, err := c.CheckHealth(healthCtx, &client.CheckHealthRequest{}); err != nil {
		c.Close()
		_ = metricsCloser.Close()

		return nil, noopShutdown, fmt.Errorf("temporal health check failed: %w", err)
	}

	closed := false
	shutdown := func() {
		if closed {
			return
		}

		closed = true

		// Close the client first so any SDK metrics emitted during its
		// teardown land in the tally scope, then close the scope to flush
		// those final samples through to the Prometheus registry.
		c.Close()
		_ = metricsCloser.Close()
	}

	return c, shutdown, nil
}

// noopShutdown is returned alongside non-nil errors so callers can always
// `defer shutdown()` without a nil check.
func noopShutdown() {}

// buildOptions loads client.Options via envconfig, replacing a file:// API
// key with dynamic credentials backed by os.ReadFile, and wiring the SDK's
// MetricsHandler/Logger to the host application's observability stack.
func buildOptions(metricsHandler client.MetricsHandler) (client.Options, error) {
	profile, err := envconfig.LoadClientConfigProfile(envconfig.LoadClientConfigProfileOptions{})
	if err != nil {
		return client.Options{}, fmt.Errorf("load temporal profile: %w", err)
	}

	apiKeyFile, err := extractAPIKeyFile(&profile)
	if err != nil {
		return client.Options{}, err
	}

	opts, err := profile.ToClientOptions(envconfig.ToClientOptionsRequest{})
	if err != nil {
		return client.Options{}, fmt.Errorf("build temporal client options: %w", err)
	}

	if apiKeyFile != "" {
		opts.Credentials = client.NewAPIKeyDynamicCredentials(apiKeyFileCallback(apiKeyFile))
	}

	opts.MetricsHandler = metricsHandler
	opts.Logger = newLogger()

	return opts, nil
}
