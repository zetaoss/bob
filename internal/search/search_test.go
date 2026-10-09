package search

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bob/internal/config"
)

func fakeEngine(t *testing.T, name, totalField string, total any, status int) Engine {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		body := map[string]any{}
		cur := body
		keys := strings.Split(totalField, ".")
		for _, k := range keys[:len(keys)-1] {
			next := map[string]any{}
			cur[k] = next
			cur = next
		}
		cur[keys[len(keys)-1]] = total
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return Engine{Name: name, BaseURL: srv.URL, QueryParam: "query", TotalField: totalField}
}

func serve(h *Handler, target string) (*httptest.ResponseRecorder, map[string]any) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestServeHTTP_CountsPerQueryAndEngine(t *testing.T) {
	h := NewHandler([]Engine{
		fakeEngine(t, "daum_blog", "meta.total_count", 576, http.StatusOK),
		fakeEngine(t, "google_search", "searchInformation.totalResults", "10700", http.StatusOK),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec, body := serve(h, "/search?q=a&q=b")

	if rec.Code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("code=%d body=%v", rec.Code, body)
	}
	result := body["result"].(map[string]any)
	if got := result["engines"].([]any); len(got) != 2 || got[0] != "daum_blog" || got[1] != "google_search" {
		t.Fatalf("engines=%v", got)
	}
	values := result["values"].([]any)
	if len(values) != 2 || values[1].([]any)[0] != float64(576) || values[1].([]any)[1] != float64(10700) {
		t.Fatalf("values=%v", values)
	}
}

func TestServeHTTP_RejectsQueryCount(t *testing.T) {
	h := NewHandler(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, target := range []string{"/search", "/search?q=a", "/search?q=a&q=b&q=c&q=d&q=e"} {
		if rec, _ := serve(h, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d, want 400", target, rec.Code)
		}
	}
}

func TestServeHTTP_EngineFailureIs500(t *testing.T) {
	h := NewHandler([]Engine{
		fakeEngine(t, "naver_blog", "total", 1, http.StatusOK),
		fakeEngine(t, "naver_book", "total", 0, http.StatusNotFound),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec, body := serve(h, "/search?q=a&q=b")

	if rec.Code != http.StatusInternalServerError || !strings.Contains(body["error"].(string), "naver_book") {
		t.Fatalf("code=%d body=%v", rec.Code, body)
	}
}

func TestEngines_OnlyConfigured(t *testing.T) {
	got := Engines(config.SearchConfig{NaverClientID: "id", NaverClientSecret: "secret", GoogleAPIKey: "k"})
	if len(got) != 2 || got[0].Name != "naver_blog" || got[1].Name != "naver_news" {
		t.Fatalf("engines=%v (google needs cx too)", got)
	}
}
