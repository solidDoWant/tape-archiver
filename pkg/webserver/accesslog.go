package webserver

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// AccessLog wraps next so every completed request emits one structured log
// record. Only r.URL.Path is logged, never the query string: /auth/callback's
// query carries the single-use OIDC code and state. Bodies, headers, and
// cookies are never logged.
//
// 5xx logs at error, 4xx at warn, else info; a request annotated via Annotate
// is raised to at least warn so a failed login (served as a 302) stays visible
// above LOG_LEVEL=info.
//
// userFor may be nil. It reads the session cookie because the identity
// pkg/webauth attaches to the downstream context is not visible out here.
func AccessLog(logger *slog.Logger, userFor func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

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

			if level < slog.LevelWarn {
				level = slog.LevelWarn
			}
		}

		logger.LogAttrs(r.Context(), level, "web: request", attrs...)
	})
}

// requestNotes is written and read only on the request's own goroutine, so it
// needs no synchronisation.
type requestNotes struct {
	attrs []slog.Attr
}

type requestNotesKey struct{}

// Annotate attaches fields explaining why the current request ended as it did
// to its AccessLog record, raising its level to at least warn. It is a no-op
// outside AccessLog; call it from the request's own goroutine.
func Annotate(ctx context.Context, attrs ...slog.Attr) {
	notes, ok := ctx.Value(requestNotesKey{}).(*requestNotes)
	if !ok {
		return
	}

	notes.attrs = append(notes.attrs, attrs...)
}

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

// remoteAddr prefers X-Forwarded-For's first entry: cmd/web always sits behind
// a TLS-terminating proxy and is not reachable except through it, so
// r.RemoteAddr is the proxy.
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

// statusRecorder captures the status and body byte count. status defaults to
// 200 (net/http's implicit status) and only the first WriteHeader wins.
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

// Unwrap lets http.ResponseController reach the real writer's Flusher; without
// it the SSE endpoint (pkg/runsapi/events.go) would buffer instead of flush.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

var _ interface{ Unwrap() http.ResponseWriter } = (*statusRecorder)(nil)
