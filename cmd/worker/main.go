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
	"os/signal"
	"syscall"

	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/internal/envvar"
	"github.com/solidDoWant/tape-archiver/pkg/logging"
	"github.com/solidDoWant/tape-archiver/pkg/metrics"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
)

// defaultMetricsAddr is the /metrics listen address used when METRICS_ADDR is
// not set. Metrics are on by default so a deployed worker is observable without
// extra configuration; set METRICS_ADDR to "" to disable the endpoint.
const defaultMetricsAddr = ":9090"

func main() {
	// ctx is cancelled on SIGINT/SIGTERM so the startup phase (e.g. a hanging
	// Temporal dial) honors shutdown signals. worker.InterruptCh, watching the
	// same signals, drives the run phase: the SDK drains the worker when it
	// yields. Both stem from one OS signal, so a single SIGTERM stops the
	// process whether it arrives during startup or steady state.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, worker.InterruptCh()); err != nil {
		// logging.Setup may not have run yet (e.g. config parse failure), so
		// write the fatal error directly to stderr rather than via slog.
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

// run parses configuration, sets up logging, metrics, and the Temporal client,
// then starts the role's worker and blocks until interruptCh yields, at which
// point the worker drains in-flight tasks before returning. Cancelling ctx
// aborts startup; interruptCh is injected so tests drive the run-phase drain
// without sending a real OS signal.
func run(ctx context.Context, interruptCh <-chan interface{}) error {
	cfg, err := parseConfig()
	if err != nil {
		return err
	}

	// Operational configuration (e.g. the failure webhook) is read from the
	// environment separately from the worker's role config so it is available to
	// the failure alert regardless of run config (SPEC §11).
	env, err := envvar.Parse()
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
		// A shutdown signal during the connect/health-check window cancels ctx
		// and surfaces here; that is an orderly stop, not a startup failure.
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer temporalShutdown()

	queue := cfg.Role.taskQueue()

	slog.Info("starting worker", "role", string(cfg.Role), "task_queue", queue)

	w := worker.New(temporalClient, queue, worker.Options{})
	registerActivities(w, cfg.Role, env)

	// Run blocks until interruptCh delivers, then stops polling and waits for
	// in-flight tasks to finish before returning.
	if err := w.Run(interruptCh); err != nil {
		return fmt.Errorf("run worker on %q task queue: %w", queue, err)
	}

	slog.Info("worker stopped", "role", string(cfg.Role), "task_queue", queue)

	return nil
}
