// Command webdevdiscord is a fake Discord webhook receiver for `make web-dev`
// (dev tooling only). It implements the three calls pkg/webhook makes: the
// ?wait=true report upload, the no-wait alert POST, and the webhook GET.
//
// A report upload whose path contains "reject" gets a permanent 403 so
// webdevseed can seed a failed run; alerts on that path are still accepted.
//
// Env: WEBDEVDISCORD_LISTEN_ADDR, WEBDEVDISCORD_GUILD_ID,
// WEBDEVDISCORD_CHANNEL_ID (all optional).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultListenAddr = "127.0.0.1:9997"

	// Syntactically valid but obviously fake snowflakes, so deep-links are
	// well-formed without addressing a real Discord resource.
	defaultGuildID   = "900000000000000001"
	defaultChannelID = "900000000000000002"
	messageIDBase    = 900000000000001000

	shutdownTimeout = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "webdevdiscord: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	addr := envOr("WEBDEVDISCORD_LISTEN_ADDR", defaultListenAddr)

	handler := newHandler(
		envOr("WEBDEVDISCORD_GUILD_ID", defaultGuildID),
		envOr("WEBDEVDISCORD_CHANNEL_ID", defaultChannelID),
	)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)

	go func() {
		serveErr <- server.Serve(listener)
	}()

	fmt.Printf("webdevdiscord: listening on %s (fake Discord webhook receiver)\n", addr)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}

		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	return nil
}

// newHandler returns the fake webhook handler; it serves every path.
func newHandler(guildID, channelID string) http.Handler {
	var messageCounter atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			slog.Info("webdevdiscord: webhook object fetched", "path", r.URL.Path)
			writeJSON(w, map[string]string{"guild_id": guildID})

		case http.MethodPost:
			// Drain the body so the client's write completes before we respond.
			_, _ = io.Copy(io.Discard, r.Body)

			if r.URL.Query().Get("wait") != "true" {
				// No-wait alert; accepted even on the reject path.
				slog.Info("webdevdiscord: alert received", "path", r.URL.Path)
				w.WriteHeader(http.StatusNoContent)

				return
			}

			if isRejectPath(r.URL.Path) {
				// 403 is non-retryable per pkg/webhook.StatusError, so Deliver fails.
				slog.Info("webdevdiscord: report upload rejected (reject endpoint)", "path", r.URL.Path)
				http.Error(w, "forbidden (fake reject endpoint)", http.StatusForbidden)

				return
			}

			messageID := fmt.Sprintf("%d", messageIDBase+messageCounter.Add(1))
			slog.Info("webdevdiscord: report delivered", "path", r.URL.Path, "message_id", messageID, "channel_id", channelID)
			writeJSON(w, map[string]string{"id": messageID, "channel_id": channelID})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	return mux
}

func isRejectPath(path string) bool {
	return strings.Contains(path, "reject")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
