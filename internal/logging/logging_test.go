package logging

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddleware_SkipsHealthz(t *testing.T) {
	var buf bytes.Buffer
	handler := Middleware(New(&buf, "info"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, path := range []string{"/healthz", "/aigate/healthz"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	if buf.Len() != 0 {
		t.Fatalf("expected health checks to be excluded from access logs, got: %q", buf.String())
	}
}

func TestMiddleware_LogsStatus(t *testing.T) {
	var buf bytes.Buffer
	handler := Middleware(New(&buf, "info"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/aigate/v1/models", nil))

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected one JSON line, got %q: %v", buf.String(), err)
	}
	if entry["msg"] != "request" || entry["path"] != "/aigate/v1/models" || entry["status"] != float64(http.StatusTeapot) {
		t.Fatalf("unexpected access log: %v", entry)
	}
}

func TestMiddleware_KeepsFlusher(t *testing.T) {
	handler := Middleware(New(&bytes.Buffer{}, "info"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through middleware failed: %v", err)
		}
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/search/", nil))
}

func TestNew_Level(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, "error")
	l.Info("hidden")
	l.Error("shown")

	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), `"msg":"shown"`) {
		t.Fatalf("unexpected output for level error: %q", buf.String())
	}
}
