// Package logging provides leveled logging and the HTTP access log.
package logging

import (
	"fmt"
	"log"
	"net/http"
	"strings"
)

type level int

const (
	levelDebug level = iota
	levelInfo
	levelError
)

// Logger writes leveled lines through the standard log package.
type Logger struct {
	level level
}

// Normalize returns the canonical form of a configured log level.
func Normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// New returns a Logger for "debug", "info" or "error"; anything else means "info".
func New(value string) *Logger {
	switch Normalize(value) {
	case "debug":
		return &Logger{level: levelDebug}
	case "error":
		return &Logger{level: levelError}
	default:
		return &Logger{level: levelInfo}
	}
}

func (l *Logger) Debugf(format string, args ...any) {
	l.printf(levelDebug, "[debug] ", format, args...)
}
func (l *Logger) Infof(format string, args ...any) { l.printf(levelInfo, "[info] ", format, args...) }
func (l *Logger) Errorf(format string, args ...any) {
	l.printf(levelError, "[error] ", format, args...)
}

func (l *Logger) printf(lv level, prefix, format string, args ...any) {
	if lv >= l.level {
		log.Print(prefix + fmt.Sprintf(format, args...))
	}
}

// Middleware access-logs every request except health checks.
func Middleware(l *Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/healthz") {
			l.Infof("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		}
		next.ServeHTTP(w, r)
	})
}
