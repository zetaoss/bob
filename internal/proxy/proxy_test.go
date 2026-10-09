package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"bob/internal/logging"
)

func TestNew_StripsPrefixAndKeepsQuery(t *testing.T) {
	var gotPath, gotQuery, gotHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotHost = r.URL.Path, r.URL.RawQuery, r.Host
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	h, err := New("search", upstream.URL, logging.New(io.Discard, "error"))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search/search?q=zetawiki", nil))

	if rec.Code != http.StatusOK || gotPath != "/search" || gotQuery != "q=zetawiki" {
		t.Fatalf("code=%d path=%q query=%q", rec.Code, gotPath, gotQuery)
	}
	if gotHost != upstream.Listener.Addr().String() {
		t.Fatalf("expected upstream Host header, got %q", gotHost)
	}
}

func TestNew_UpstreamDownIs502(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	h, err := New("runbox", down.URL, logging.New(io.Discard, "error"))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runbox/lang", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}
