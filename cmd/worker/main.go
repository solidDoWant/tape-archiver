// Command worker is the Temporal worker for tape-archiver; ROLE selects the
// control or data worker (SPEC §4.1).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/worker"

	"github.com/solidDoWant/tape-archiver/internal/buildinfo"
	"github.com/solidDoWant/tape-archiver/internal/envvar"
	"github.com/solidDoWant/tape-archiver/pkg/health"
	"github.com/solidDoWant/tape-archiver/pkg/logging"
	"github.com/solidDoWant/tape-archiver/pkg/metrics"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
)

// defaultMetricsAddr applies when METRICS_ADDR is unset; "" disables /metrics.
const defaultMetricsAddr = ":9090"

// defaultHealthAddr applies when HEALTH_ADDR is unset; "" disables the probes.
// It is a separate port so health survives /metrics being disabled.
const defaultHealthAddr = ":8080"

func main() {
	// Container HEALTHCHECK entrypoint: probe and exit without starting a worker.
	if len(os.Args) > 1 && os.Args[1] == healthcheckSubcommand {
		if err := runHealthcheck(context.Background(), os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "worker: %v\n", err)
			os.Exit(1)
		}

		return
	}

	// ctx covers startup (e.g. a hanging Temporal dial); worker.InterruptCh
	// drives the run-phase drain. One SIGTERM stops either phase.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, worker.InterruptCh()); err != nil {
		// logging.Setup may not have run yet, so bypass slog.
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

// workerOptions returns the worker options for role. The data worker enables
// sessions so write-phase activities pin to one process (SPEC §4.3 phase 7).
// activityLogInterceptor must follow logTagsInterceptor so its lines inherit
// the run tags (#303).
func workerOptions(role Role) worker.Options {
	options := worker.Options{
		Interceptors: []interceptor.WorkerInterceptor{
			&logTagsInterceptor{},
			newActivityLogInterceptor(role),
		},
	}

	if role == RoleData {
		options.EnableSessionWorker = true
	}

	return options
}

// run starts the role's worker and blocks until interruptCh yields, then drains.
// Cancelling ctx aborts startup; interruptCh is injectable for tests.
func run(ctx context.Context, interruptCh <-chan interface{}) error {
	cfg, err := parseConfig()
	if err != nil {
		return err
	}

	// Read separately from the run config so the failure alert works
	// regardless of it (SPEC §11).
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

	// A nil registerer (metrics disabled) turns SDK metrics off.
	temporalClient, temporalShutdown, err := temporalclient.New(ctx, metricsProvider.PrometheusRegisterer())
	if err != nil {
		// A shutdown signal during connect is an orderly stop.
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer temporalShutdown()

	// Readiness re-checks Temporal per probe so a lost connection reports
	// not-ready without exiting.
	_, healthShutdown, err := health.NewFromEnv(defaultHealthAddr, func(ctx context.Context) error {
		_, err := temporalClient.CheckHealth(ctx, &client.CheckHealthRequest{})

		return err
	})
	if err != nil {
		return fmt.Errorf("set up health server: %w", err)
	}
	defer healthShutdown()

	queue := cfg.Role.taskQueue()

	slog.Info("starting worker",
		"role", string(cfg.Role), "task_queue", queue, "version", buildinfo.ToolVersion())

	options := workerOptions(cfg.Role)

	// Control-role idle exit lets a KEDA-spawned Job scale to zero (SPEC §4.1; #113).
	idleCtx, cancelIdle := context.WithCancel(ctx)
	defer cancelIdle()

	workerInterruptCh, err := installIdleExit(idleCtx, cfg, &options, metricsProvider.PrometheusRegisterer(), interruptCh)
	if err != nil {
		return err
	}

	w := worker.New(temporalClient, queue, options)
	registerActivities(w, cfg.Role, env, metricsProvider.PrometheusRegisterer())

	if err := w.Run(workerInterruptCh); err != nil {
		return fmt.Errorf("run worker on %q task queue: %w", queue, err)
	}

	slog.Info("worker stopped", "role", string(cfg.Role), "task_queue", queue)

	// Allow a final scrape before metricsShutdown. ctx is already cancelled by
	// SIGTERM, so a fresh context is required; a timeout is benign.
	if err := metricsProvider.WaitForScrape(context.Background()); err != nil {
		slog.Debug("final metrics scrape wait ended without a scrape", "error", err)
	}

	return nil
}
