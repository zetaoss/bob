package logging

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	originalOutput := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(originalOutput)
		log.SetFlags(originalFlags)
	})
	return &buf
}

func TestMiddleware_SkipsHealthz(t *testing.T) {
	buf := captureLog(t)
	handler := Middleware(New("info"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, path := range []string{"/healthz", "/aigate/healthz"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	if strings.Contains(buf.String(), "healthz") {
		t.Fatalf("expected health checks to be excluded from access logs, got: %q", buf.String())
	}
}

func TestMiddleware_LogsOtherRequests(t *testing.T) {
	buf := captureLog(t)
	handler := Middleware(New("info"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/aigate/v1/models", nil))

	if !strings.Contains(buf.String(), "GET /aigate/v1/models") {
		t.Fatalf("expected request to be access-logged, got: %q", buf.String())
	}
}

func TestLevels(t *testing.T) {
	buf := captureLog(t)
	l := New("error")
	l.Infof("hidden")
	l.Errorf("shown")

	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "[error] shown") {
		t.Fatalf("unexpected output for level error: %q", buf.String())
	}
}
