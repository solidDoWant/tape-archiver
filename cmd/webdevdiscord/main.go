// Command webdevdiscord is a local, fake Discord webhook receiver for `make
// web-dev` (issue #313) — dev tooling only, never built into a shipped image
// or run in CI. It stands in for Discord's webhook API so a local backup can
// actually deliver its PDF report and the web UI's "Discord report ↗"
// deep-link (issue #306) renders, instead of every delivery failing against a
// placeholder discord.com URL that would reject the POST.
//
// It implements exactly the three calls pkg/webhook makes (see
// pkg/webhook/webhook.go), and nothing else:
//
//   - POST {webhook}?wait=true — the multipart report upload (Client.SendFile).
//     Responds 200 with a Discord-shaped message object {"id","channel_id"} so
//     the deep-link identity resolves.
//   - POST {webhook} — a JSON content alert (Client.Send: failure / pause
//     notifications). Responds 204, mirroring Discord's bare no-wait execution.
//   - GET {webhook} — the webhook object fetch (Client.FetchWebhookGuild).
//     Responds 200 with {"guild_id"} so the deep-link can name the guild.
//
// One exception: a report upload whose path contains "reject" (webdevseed's
// WEBDEVSEED_FAIL_WEBHOOK_URL) is answered with a permanent 403, so the Deliver
// phase treats it as a non-retryable rejection and the run fails — this is how
// `make web-dev` seeds a deliberately-failed run. The no-wait alert on that same
// path is still accepted (204), so the failed run's SPEC §11 alert still lands.
//
// The IDs are fixed, syntactically-valid snowflakes so the constructed
// https://discord.com/channels/{guild}/{channel}/{message} link is well-formed;
// each delivered report gets a distinct message id from an in-process counter.
// The link will NOT resolve on real Discord — these are dev fakes — but it
// demonstrates the feature end-to-end locally. Every request is logged.
//
// Configuration is entirely via environment variables (all optional, with
// dev-friendly defaults — this is never meant to be reachable from anywhere but
// a developer's own machine):
//
//   - WEBDEVDISCORD_LISTEN_ADDR — address to listen on (default "127.0.0.1:9997").
//   - WEBDEVDISCORD_GUILD_ID / WEBDEVDISCORD_CHANNEL_ID — the guild/channel
//     snowflakes the deep-link is built from (dev-friendly defaults below).
//
// See docs/web-ui.md's "Local development" section.
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

	// defaultGuildID / defaultChannelID / messageIDBase are fixed,
	// syntactically-valid Discord snowflakes (19-digit) so the reconstructed
	// jump-to-message URL is well-formed. They are deliberately in a high,
	// obviously-fake range — these never address a real Discord resource.
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

// newHandler returns the fake webhook handler serving every path (the webhook
// URL's path is arbitrary — pkg/webhook posts/gets whatever URL it was given).
// It is split out from run so tests can exercise the three routes against an
// httptest server without binding a fixed port.
func newHandler(guildID, channelID string) http.Handler {
	// messageCounter hands each delivered report a distinct message snowflake,
	// so multiple seeded runs produce distinct (well-formed) deep-links.
	var messageCounter atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// FetchWebhookGuild: the webhook object, carrying its guild.
			slog.Info("webdevdiscord: webhook object fetched", "path", r.URL.Path)
			writeJSON(w, map[string]string{"guild_id": guildID})

		case http.MethodPost:
			// Drain the body (a multipart report upload or a JSON alert) so the
			// client's write always completes before we respond.
			_, _ = io.Copy(io.Discard, r.Body)

			if r.URL.Query().Get("wait") != "true" {
				// A no-wait alert (Client.Send): Discord answers 204 with no body.
				// Accepted even on the reject path, so a run failed via a rejected
				// report upload still lands its SPEC §11 failure alert here.
				slog.Info("webdevdiscord: alert received", "path", r.URL.Path)
				w.WriteHeader(http.StatusNoContent)

				return
			}

			if isRejectPath(r.URL.Path) {
				// The reject endpoint (webdevseed's WEBDEVSEED_FAIL_WEBHOOK_URL):
				// answer a permanent 4xx so the Deliver phase treats it as a
				// deterministic, non-retryable rejection and fails the run. A 403
				// stands in for Discord refusing the upload (a deleted/forbidden
				// webhook) — a non-retryable status per pkg/webhook.StatusError.
				slog.Info("webdevdiscord: report upload rejected (reject endpoint)", "path", r.URL.Path)
				http.Error(w, "forbidden (fake reject endpoint)", http.StatusForbidden)

				return
			}

			// SendFile's ?wait=true report upload: return a message object so the
			// deep-link identity (channel + message) resolves.
			messageID := fmt.Sprintf("%d", messageIDBase+messageCounter.Add(1))
			slog.Info("webdevdiscord: report delivered", "path", r.URL.Path, "message_id", messageID, "channel_id", channelID)
			writeJSON(w, map[string]string{"id": messageID, "channel_id": channelID})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	return mux
}

// isRejectPath reports whether a webhook path is the deliberate-failure endpoint
// (webdevseed points its seeded-to-fail runs at ".../webhook/reject"). Matched by
// substring so any path a developer routes there works, not one hard-coded route.
func isRejectPath(path string) bool {
	return strings.Contains(path, "reject")
}

// writeJSON writes v as a 200 JSON response, matching the content type Discord
// returns so the webhook client's json.Unmarshal path is exercised as in prod.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// envOr reads the named environment variable, returning fallback when it is
// unset or empty.
func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
