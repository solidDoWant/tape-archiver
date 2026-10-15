// Package temporalclient builds an envconfig-configured Temporal client. A
// "file:///path" API key is re-read on every RPC so rotation needs no restart,
// and [New] health-checks the connection because [client.Dial] is lazy.
package temporalclient

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"
)

const healthCheckTimeout = 10 * time.Second

// New dials and health-checks a Temporal client, registering SDK metrics on
// reg (nil disables them). The returned shutdown is idempotent and safe to
// defer even on error.
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

		// Close the client first so its teardown metrics are flushed.
		c.Close()
		_ = metricsCloser.Close()
	}

	return c, shutdown, nil
}

func noopShutdown() {}

const defaultNamespace = "default"

// ResolveNamespace returns the namespace the envconfig profile used by New
// resolves to.
func ResolveNamespace() (string, error) {
	profile, err := envconfig.LoadClientConfigProfile(envconfig.LoadClientConfigProfileOptions{})
	if err != nil {
		return "", fmt.Errorf("load temporal profile: %w", err)
	}

	opts, err := profile.ToClientOptions(envconfig.ToClientOptionsRequest{})
	if err != nil {
		return "", fmt.Errorf("build temporal client options: %w", err)
	}

	if opts.Namespace == "" {
		return defaultNamespace, nil
	}

	return opts.Namespace, nil
}

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
