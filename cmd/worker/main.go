// Command worker is the Temporal worker for tape-archiver. A single binary
// serves both roles; the ROLE environment variable selects whether it runs as
// the control worker (Kubernetes-side: snapshot resolution, report/ISO
// building, Discord delivery) or the data worker (storage-host-side:
// tar/age/PAR2/checksum/LTFS/changer activities). See SPEC §4.1.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/pkg/logging"
	"github.com/solidDoWant/tape-archiver/pkg/metrics"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
)

// defaultMetricsAddr is the /metrics listen address used when METRICS_ADDR is
// not set. Metrics are on by default so a deployed worker is observable without
// extra configuration; set METRICS_ADDR to "" to disable the endpoint.
const defaultMetricsAddr = ":9090"

func main() {
	// main wires the OS interrupt channel to run, which blocks until SIGTERM/
	// SIGINT and then drains the worker gracefully.
	if err := run(context.Background(), worker.InterruptCh()); err != nil {
		// logging.Setup may not have run yet (e.g. config parse failure), so
		// write the fatal error directly to stderr rather than via slog.
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

// run parses configuration, sets up logging, metrics, and the Temporal client,
// then starts the role's worker and blocks until interruptCh receives a value,
// at which point the worker drains in-flight tasks before returning. The
// interrupt channel is injected so tests can drive a controlled shutdown.
func run(ctx context.Context, interruptCh <-chan interface{}) error {
	cfg, err := parseConfig()
	if err != nil {
		return err
	}

	logging.Setup(cfg.LogLevel)

	metricsProvider, metricsShutdown, err := metrics.NewFromEnv(defaultMetricsAddr)
	if err != nil {
		return fmt.Errorf("set up metrics: %w", err)
	}
	defer metricsShutdown()

	// SDK metrics register against the same Prometheus registry as application
	// metrics so they share one /metrics endpoint. The registerer is nil when
	// the metrics endpoint is disabled, which temporalclient.New treats as
	// "SDK metrics off".
	temporalClient, temporalShutdown, err := temporalclient.New(ctx, metricsProvider.PrometheusRegisterer())
	if err != nil {
		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer temporalShutdown()

	queue := cfg.Role.taskQueue()

	slog.Info("starting worker", "role", string(cfg.Role), "task_queue", queue)

	w := worker.New(temporalClient, queue, worker.Options{})
	registerActivities(w, cfg.Role)

	// Run blocks until interruptCh delivers, then stops polling and waits for
	// in-flight tasks to finish before returning.
	if err := w.Run(interruptCh); err != nil {
		return fmt.Errorf("run worker on %q task queue: %w", queue, err)
	}

	slog.Info("worker stopped", "role", string(cfg.Role), "task_queue", queue)

	return nil
}
