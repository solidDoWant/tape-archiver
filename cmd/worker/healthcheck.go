package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// healthcheckSubcommand is the argv[1] value that selects the self-probe path
// instead of the normal worker path.
const healthcheckSubcommand = "healthcheck"

// healthcheckTimeout bounds a single self-probe request. It is comfortably under
// a typical container HEALTHCHECK timeout so the probe fails fast rather than
// hanging the health-check runner.
const healthcheckTimeout = 3 * time.Second

// runHealthcheck is the `worker healthcheck` self-probe used as the container
// HEALTHCHECK. It GETs /readyz on the local health server and returns an error
// (mapped to a non-zero exit) unless the server responds 200. Container health
// therefore reflects readiness — a worker that has lost its Temporal connection
// reports unhealthy even though its process is still alive.
//
// It never starts a Temporal worker: main dispatches here before any worker
// setup, and this function only makes a local HTTP request.
//
// The target address is the local health endpoint. It defaults to the same
// HEALTH_ADDR the server binds (or defaultHealthAddr when unset); an optional
// positional argument overrides it for testing or non-standard setups.
func runHealthcheck(ctx context.Context, args []string) error {
	addr := resolveHealthAddr(args)
	if addr == "" {
		return fmt.Errorf("healthcheck: no health address configured (HEALTH_ADDR is empty)")
	}

	url := "http://" + addr + "/readyz"

	reqCtx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: build request for %s: %w", url, err)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("healthcheck: probe %s: %w", url, err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s returned status %d (not ready)", url, response.StatusCode)
	}

	return nil
}

// resolveHealthAddr picks the address the self-probe targets. An explicit
// positional argument (args[0]) wins; otherwise HEALTH_ADDR is used, falling
// back to defaultHealthAddr when HEALTH_ADDR is unset. A bare or wildcard host
// (e.g. ":8080" or "0.0.0.0:8080") is rewritten to loopback because the probe
// dials the server inside the same container.
func resolveHealthAddr(args []string) string {
	addr := os.Getenv("HEALTH_ADDR")
	if _, set := os.LookupEnv("HEALTH_ADDR"); !set {
		addr = defaultHealthAddr
	}

	if len(args) > 0 && args[0] != "" {
		addr = args[0]
	}

	if addr == "" {
		return ""
	}

	return loopbackAddr(addr)
}

// loopbackAddr rewrites a listen address into one dialable from the same host.
// A missing or unspecified host (empty or 0.0.0.0 / ::) becomes 127.0.0.1, so a
// server that binds all interfaces is still reachable by the self-probe.
func loopbackAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not host:port (e.g. a bare port); leave it for the HTTP client to
		// report a clear dial error rather than silently rewriting.
		return addr
	}

	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	return net.JoinHostPort(host, port)
}
