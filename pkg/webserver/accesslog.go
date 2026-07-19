package webserver

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// AccessLog wraps next so every completed HTTP request emits exactly one
// structured log record — the operator-facing access log cmd/web otherwise
// lacks (before this, only server lifecycle and ad-hoc handler errors were
// logged, so there was no record of who requested what, with what result).
// cmd/web mounts it outermost (around SecurityHeaders and pkg/webauth's Wrap)
// so a record is produced for every response the server sends: the SPA, its
// assets, /api/*, the /auth/* routes, and the 401/403 a gate returns.
//
// Each record carries the request method, the URL path, the response status
// and byte count, the elapsed time, the client address, and — when userFor
// reports one — the authenticated user. Two things are deliberately kept out
// of it:
//
//   - The query string is never logged, only r.URL.Path. /auth/callback's
//     query carries the OIDC authorization code and state; logging the raw
//     query would spill those single-use secrets into the log. Run IDs, which
//     appear in the path (/api/runs/{id}), are logged — an access log is meant
//     to record which resource was touched, and a run ID is not a secret.
//   - No request or response body, headers, or cookies — an access log records
//     that a request happened and how it ended, not its contents.
//
// The record's level tracks the status so failures stay visible even when the
// deployment raises LOG_LEVEL above info: 5xx logs at error, 4xx at warn,
// everything else at info. A handler can additionally attach fields explaining
// *why* a request ended as it did — the reason for a 401/403, the IdP error
// behind a failed /auth/callback — with Annotate; a request carrying any such
// annotation is treated as noteworthy and logged at warn-or-higher even when
// its status alone (e.g. the 302 the OIDC login callback redirects with on
// failure) would be info. This is what lets the access log, on its own,
// explain an otherwise-opaque login failure.
//
// userFor may be nil (no user attribution). It is called once per request,
// after next returns; cmd/web passes a closure over
// webauth.Authenticator.IdentityFromRequest, which reads the request's session
// cookie — the identity pkg/webauth attaches to the *downstream* request
// context is not visible to this outer middleware, so attribution is sourced
// from the cookie directly rather than from that context.
func AccessLog(logger *slog.Logger, userFor func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// Install the per-request annotation sink so handlers below can call
		// Annotate to explain the outcome; read back after next returns.
		notes := &requestNotes{}
		r = r.WithContext(context.WithValue(r.Context(), requestNotesKey{}, notes))

		next.ServeHTTP(recorder, r)

		elapsed := time.Since(start)

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.status),
			slog.Int("bytes", recorder.bytes),
			slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
			slog.String("remote", remoteAddr(r)),
		}

		if userFor != nil {
			if user := userFor(r); user != "" {
				attrs = append(attrs, slog.String("user", user))
			}
		}

		level := levelForStatus(recorder.status)

		if len(notes.attrs) > 0 {
			attrs = append(attrs, notes.attrs...)

			// An annotated request is one a handler flagged as noteworthy (it
			// recorded why the request ended as it did), so surface it even at a
			// raised LOG_LEVEL — a failing login callback redirects 302, whose
			// status alone would keep it at info and hide it at LOG_LEVEL=warn.
			if level < slog.LevelWarn {
				level = slog.LevelWarn
			}
		}

		logger.LogAttrs(r.Context(), level, "web: request", attrs...)
	})
}

// requestNotes is the per-request sink AccessLog installs in the request
// context for Annotate to append to. It is written only from the request's own
// handler goroutine (before next returns) and read only after next returns, in
// the same goroutine, so it needs no synchronisation.
type requestNotes struct {
	attrs []slog.Attr
}

// requestNotesKey is an unexported context key type, so the sink AccessLog
// stores can never collide with another package's context value.
type requestNotesKey struct{}

// Annotate attaches structured fields to the access-log record AccessLog will
// emit for the current request, so a handler can record *why* a request ended
// as it did — the reason a gate returned 401/403, the identity provider's
// error behind a failed /auth/callback — without standing up its own logging.
// A request carrying any annotation is logged at warn-or-higher regardless of
// status, so a failure that is served as a redirect (the OIDC callback's 302)
// is not hidden when the deployment runs above info.
//
// It is a no-op when ctx is not from a request wrapped by AccessLog (e.g. a
// unit test exercising a handler directly), so callers never have to guard the
// call. Call it from the request's own handler goroutine.
func Annotate(ctx context.Context, attrs ...slog.Attr) {
	notes, ok := ctx.Value(requestNotesKey{}).(*requestNotes)
	if !ok {
		return
	}

	notes.attrs = append(notes.attrs, attrs...)
}

// levelForStatus maps a response status to the level its access-log record is
// emitted at, so a failing request is not silently swallowed when the
// deployment runs above info: server errors at error, client errors at warn,
// and successful/redirect responses at info.
func levelForStatus(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// remoteAddr returns the client address to log. In this app's deployment model
// cmd/web always sits behind a TLS-terminating proxy (see webauth.isTLS's note
// on X-Forwarded-Proto), so r.RemoteAddr is the proxy's address, not the
// operator's. X-Forwarded-For's first entry is the original client, trusted
// here on the same basis every other forwarded header already is — the pod is
// not expected to be reachable except through that proxy. With no such header
// (a direct connection, e.g. in tests or a non-proxied local run) r.RemoteAddr
// is the real peer and is used as-is.
func remoteAddr(r *http.Request) string {
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return r.RemoteAddr
	}

	if comma := strings.IndexByte(forwarded, ','); comma >= 0 {
		forwarded = forwarded[:comma]
	}

	return strings.TrimSpace(forwarded)
}

// statusRecorder wraps an http.ResponseWriter to capture the status code and
// number of body bytes written, so AccessLog can report both after the
// handler has run. A handler that writes a body without calling WriteHeader
// implicitly sends 200, matching net/http's own behaviour, so status defaults
// to http.StatusOK and only the first WriteHeader wins.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if !s.wroteHeader {
		s.status = status
		s.wroteHeader = true
	}

	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true

	written, err := s.ResponseWriter.Write(b)
	s.bytes += written

	return written, err
}

// Unwrap exposes the underlying ResponseWriter to http.ResponseController.
// The SSE endpoint (pkg/runsapi/events.go) drives its stream through
// http.NewResponseController(w).Flush / .SetWriteDeadline; without Unwrap the
// controller could not see past this wrapper to the real writer's Flusher, and
// the live run-monitoring stream would buffer instead of flushing. Preserving
// it is why AccessLog can safely wrap the whole handler, SSE routes included.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// compile-time assurance that a *statusRecorder can be unwrapped by
// http.ResponseController (which looks for exactly this method).
var _ interface{ Unwrap() http.ResponseWriter } = (*statusRecorder)(nil)
