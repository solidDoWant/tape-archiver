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

const healthcheckSubcommand = "healthcheck"

// healthcheckTimeout stays under a typical container HEALTHCHECK timeout.
const healthcheckTimeout = 3 * time.Second

// runHealthcheck is the container HEALTHCHECK self-probe: it fails unless the
// local /readyz returns 200, so container health reflects Temporal readiness.
// An optional positional argument overrides the probed address.
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

// resolveHealthAddr returns args[0], else HEALTH_ADDR, else defaultHealthAddr,
// rewritten to loopback.
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

// loopbackAddr maps an empty or wildcard host to 127.0.0.1.
func loopbackAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Let the HTTP client report a clear dial error.
		return addr
	}

	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	return net.JoinHostPort(host, port)
}
