// Package logging builds the JSON slog logger and the HTTP access log.
package logging

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Normalize returns the canonical form of a configured log level.
func Normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// New returns a JSON logger writing to w at "debug", "info" or "error"; anything else means "info".
func New(w io.Writer, level string) *slog.Logger {
	lv := slog.LevelInfo
	switch Normalize(level) {
	case "debug":
		lv = slog.LevelDebug
	case "error":
		lv = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}

// Middleware access-logs every request except health checks, with status and duration.
func Middleware(l *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/healthz") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		l.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush on the underlying writer (streaming proxies).
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
