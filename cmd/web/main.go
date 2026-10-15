// Command web serves the tape-archiver web UI: the embedded React SPA at "/"
// and the JSON API (pkg/runsapi) under "/api/*", gated behind mandatory OIDC
// authentication (pkg/webauth). Its environment variables are documented in
// docs/configuration.md. HEALTH_ADDR defaults to ":8081" rather than
// cmd/worker's ":8080" because the main port already serves real traffic.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/solidDoWant/tape-archiver/internal/buildinfo"
	"github.com/solidDoWant/tape-archiver/pkg/health"
	"github.com/solidDoWant/tape-archiver/pkg/logging"
	"github.com/solidDoWant/tape-archiver/pkg/metrics"
	"github.com/solidDoWant/tape-archiver/pkg/runsapi"
	"github.com/solidDoWant/tape-archiver/pkg/temporalclient"
	"github.com/solidDoWant/tape-archiver/pkg/webauth"
	"github.com/solidDoWant/tape-archiver/pkg/webserver"
)

const defaultListenAddr = ":8080"

const defaultHealthAddr = ":8081"

const defaultMetricsAddr = ":9090"

// Timeouts guard against slow clients (e.g. Slowloris) holding connections open.
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

// run serves until ctx is cancelled, then shuts down with a bounded grace
// period. ready, if non-nil, receives the bound main address (tests use ":0").
func run(ctx context.Context, getenv func(string) string, ready func(addr string)) error {
	logging.Setup(getenv("LOG_LEVEL"))

	// Deferred first so it runs last and logs the total shutdown time.
	var shutdownStart time.Time

	defer func() {
		if !shutdownStart.IsZero() {
			// .String() logs "1.2s" rather than raw nanoseconds.
			slog.Info("web: shutdown complete", "total", time.Since(shutdownStart).Round(time.Millisecond).String())
		}
	}()

	assets, err := distFS()
	if err != nil {
		return fmt.Errorf("load embedded SPA assets: %w", err)
	}

	// Unlike cmd/worker, a long-running server loses at most one scrape
	// interval at exit, so skip the final-scrape wait by default (issue #270).
	metricsProvider, metricsShutdown, err := metrics.NewFromEnv(defaultMetricsAddr, metrics.WithScrapeWaitTimeout(0))
	if err != nil {
		return fmt.Errorf("set up metrics: %w", err)
	}
	defer logShutdownStage("metrics server shutdown", metricsShutdown)

	temporalClient, temporalShutdown, err := temporalclient.New(ctx, metricsProvider.PrometheusRegisterer())
	if err != nil {
		// A shutdown signal during connect is an orderly stop.
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer logShutdownStage("temporal client shutdown", temporalShutdown)

	_, healthShutdown, err := health.NewFromEnv(defaultHealthAddr, func(ctx context.Context) error {
		_, err := temporalClient.CheckHealth(ctx, &client.CheckHealthRequest{})

		return err
	})
	if err != nil {
		return fmt.Errorf("set up health server: %w", err)
	}
	// Shut health down as soon as the main listener stops, not at the end of
	// run(): otherwise /readyz keeps reporting 200 through the scrape wait
	// and Kubernetes keeps routing traffic to a pod no longer serving.
	healthShutdownPending := true

	defer func() {
		if healthShutdownPending {
			logShutdownStage("health server shutdown", healthShutdown)
		}
	}()

	oidcConfig, err := oidcConfigFromEnv(getenv)
	if err != nil {
		return fmt.Errorf("configure OIDC: %w", err)
	}

	authenticator, err := webauth.New(ctx, oidcConfig)
	if err != nil {
		// Shutdown during OIDC discovery is an orderly stop.
		if ctx.Err() != nil {
			return nil
		}

		return fmt.Errorf("set up OIDC authenticator: %w", err)
	}

	// drainCtx ends SSE streams when shutdown begins; an active SSE response
	// never goes idle, so srv.Shutdown would otherwise hit its deadline
	// (issue #270).
	drainCtx, startDrain := context.WithCancel(context.Background())
	defer startDrain()

	// Optional Temporal UI deep-links; unset TEMPORAL_UI_URL shows no link.
	temporalUIURL := getenv("TEMPORAL_UI_URL")
	temporalNamespace := ""

	if temporalUIURL != "" {
		temporalNamespace, err = temporalclient.ResolveNamespace()
		if err != nil {
			// A convenience link is not worth failing startup over.
			slog.Warn("web: disabling Temporal UI deep-links — could not resolve namespace", "error", err)

			temporalUIURL = ""
		}
	}

	// Device paths, webhook URL, and library topology are host properties, not
	// per-run choices (issues #304, #305, #317). All are optional.
	deployChanger := getenv("LIBRARY_CHANGER")
	deployDrives := splitDeviceList(getenv("LIBRARY_DRIVES"))
	deployWebhookURL := getenv("DELIVERY_WEBHOOK_URL")

	deployOpticalBurnerDrives := splitDeviceList(getenv("OPTICAL_BURNER_DRIVES"))

	// A malformed slot count degrades to 0 ("not configured") rather than
	// failing startup.
	deploySlotCount, _ := strconv.Atoi(strings.TrimSpace(getenv("LIBRARY_SLOT_COUNT")))
	deployCleaningSlots := splitIntList(getenv("LIBRARY_CLEANING_SLOTS"))
	deployIOStationSlots := splitIntList(getenv("LIBRARY_IO_STATION_SLOTS"))

	handler, err := webserver.NewHandler(assets, runsapi.New(
		temporalClient,
		runsapi.WithDrainContext(drainCtx),
		runsapi.WithTemporalUI(temporalUIURL, temporalNamespace),
		runsapi.WithDeployConfig(deployChanger, deployDrives, deployWebhookURL),
		runsapi.WithOpticalBurnerDrives(deployOpticalBurnerDrives),
		runsapi.WithLibraryTopology(deploySlotCount, deployCleaningSlots, deployIOStationSlots),
	))
	if err != nil {
		return fmt.Errorf("build web server handler: %w", err)
	}

	handler = authenticator.Wrap(handler)

	handler = webserver.SecurityHeaders(handler)

	// The identity Wrap attaches to the downstream context is not visible out
	// here, so read the subject from the cookie directly.
	handler = webserver.AccessLog(slog.Default(), func(r *http.Request) string {
		if identity, ok := authenticator.IdentityFromRequest(r); ok {
			return identity.Subject
		}

		return ""
	}, handler)

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

	shutdownStart = time.Now()

	startDrain()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	drainStart := time.Now()
	shutdownErr := srv.Shutdown(shutdownCtx)

	slog.Info("web: shutdown stage complete", "stage", "main server drain", "duration", time.Since(drainStart).Round(time.Millisecond).String())

	if shutdownErr != nil {
		return fmt.Errorf("shutdown: %w", shutdownErr)
	}

	// Surface a serve error that raced the shutdown signal, so a real failure
	// does not exit 0.
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	logShutdownStage("health server shutdown", healthShutdown)

	healthShutdownPending = false

	// ctx is already cancelled here, so the wait needs a fresh context.
	logShutdownStage("final metrics scrape wait", func() {
		if err := metricsProvider.WaitForScrape(context.Background()); err != nil {
			slog.Debug("final metrics scrape wait ended without a scrape", "error", err)
		}
	})

	return nil
}

// logShutdownStage runs stage and logs its duration, so a slow exit can be
// attributed to a specific stage (issue #270).
func logShutdownStage(name string, stage func()) {
	start := time.Now()

	stage()

	slog.Info("web: shutdown stage complete", "stage", name, "duration", time.Since(start).Round(time.Millisecond).String())
}

// splitDeviceList splits a comma-separated list, dropping blanks; never nil.
func splitDeviceList(value string) []string {
	drives := make([]string, 0)

	for _, entry := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			drives = append(drives, trimmed)
		}
	}

	return drives
}

// splitIntList splits a comma-separated int list, skipping malformed entries.
// It never returns nil so the API reports [] rather than null.
func splitIntList(value string) []int {
	slots := make([]int, 0)

	for _, entry := range strings.Split(value, ",") {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}

		if slot, err := strconv.Atoi(trimmed); err == nil {
			slots = append(slots, slot)
		}
	}

	return slots
}

func listenAddr(getenv func(string) string) string {
	if addr := getenv("WEB_LISTEN_ADDRESS"); addr != "" {
		return addr
	}

	return defaultListenAddr
}

// oidcConfigFromEnv builds a webauth.Config; webauth.New validates it.
func oidcConfigFromEnv(getenv func(string) string) (webauth.Config, error) {
	sessionKey, err := webauth.ParseSessionKey(getenv("WEB_SESSION_KEY"))
	if err != nil {
		return webauth.Config{}, err
	}

	return webauth.Config{
		IssuerURL:    getenv("OIDC_ISSUER_URL"),
		ClientID:     getenv("OIDC_CLIENT_ID"),
		ClientSecret: getenv("OIDC_CLIENT_SECRET"),
		RedirectURL:  getenv("OIDC_REDIRECT_URL"),
		SessionKey:   sessionKey,
		// AppVersion is the build's embedded VCS version, not an env var.
		AppVersion: buildinfo.ToolVersion(),
		FooterHost: getenv("WEB_FOOTER_HOST"),
	}, nil
}
