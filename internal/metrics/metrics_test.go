package metrics

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"bob/internal/config"
)

func fakePrometheus(t *testing.T, gotTime *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotTime != nil {
			*gotTime = r.URL.Query().Get("time")
		}
		var body string
		switch r.URL.Query().Get("query") {
		case "per_node":
			body = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"node":"n1"},"value":[1,"0.5"]},{"metric":{"node":"n2"},"value":[1,"0.25"]}]}}`
		case "one":
			body = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"10"]}]}}`
		case "scalar":
			body = `{"status":"success","data":{"resultType":"scalar","result":[1,"3"]}}`
		case "nan":
			body = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"NaN"]}]}}`
		case "empty":
			body = `{"status":"success","data":{"resultType":"vector","result":[]}}`
		default:
			w.WriteHeader(http.StatusBadRequest)
			body = `{"status":"error","error":"parse error"}`
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newHandler(prom string, queries map[string]string) *Handler {
	return NewHandler(config.MetricsConfig{Prometheus: prom, Queries: queries}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

type body struct {
	Status string              `json:"status"`
	Time   string              `json:"time"`
	Result map[string][]Sample `json:"result"`
	Error  string              `json:"error"`
}

func get(t *testing.T, h http.Handler, target string) (int, body) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var b body
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return rec.Code, b
}

func TestServeHTTP_All(t *testing.T) {
	var gotTime string
	h := newHandler(fakePrometheus(t, &gotTime), map[string]string{
		"node_cpu_usage": "per_node", "pod_count": "one", "level": "scalar", "ratio": "nan", "none": "empty",
	})

	code, b := get(t, h, "/?time=2026-10-09T07:00:00Z")

	if code != http.StatusOK || b.Status != "ok" || b.Time != "2026-10-09T07:00:00Z" || gotTime != "2026-10-09T07:00:00Z" {
		t.Fatalf("code=%d body=%+v gotTime=%q", code, b, gotTime)
	}
	if got := b.Result["node_cpu_usage"]; len(got) != 2 || got[0].Labels["node"] != "n1" || got[1].Value != 0.25 {
		t.Errorf("node_cpu_usage=%+v", got)
	}
	if got := b.Result["pod_count"]; len(got) != 1 || got[0].Value != 10 {
		t.Errorf("pod_count=%+v", got)
	}
	if got := b.Result["level"]; len(got) != 1 || got[0].Value != 3 {
		t.Errorf("scalar=%+v", got)
	}
	if got, ok := b.Result["ratio"]; !ok || len(got) != 0 {
		t.Errorf("NaN should be dropped: %+v", got)
	}
	if got, ok := b.Result["none"]; !ok || len(got) != 0 {
		t.Errorf("empty=%+v", got)
	}
}

func TestServeHTTP_One(t *testing.T) {
	h := newHandler(fakePrometheus(t, nil), map[string]string{"pod_count": "one", "node_cpu_usage": "per_node"})

	code, b := get(t, h, "/pod_count")
	if code != http.StatusOK || len(b.Result) != 1 || b.Result["pod_count"][0].Value != 10 {
		t.Fatalf("code=%d body=%+v", code, b)
	}
	if code, _ := get(t, h, "/unknown"); code != http.StatusNotFound {
		t.Errorf("unknown metric: code=%d", code)
	}
	if code, _ := get(t, h, "/?time=yesterday"); code != http.StatusBadRequest {
		t.Errorf("bad time: code=%d", code)
	}
}

func TestServeHTTP_QueryErrorIs502(t *testing.T) {
	h := newHandler(fakePrometheus(t, nil), map[string]string{"ok": "one", "broken": "syntax error("})

	code, b := get(t, h, "/")
	if code != http.StatusBadGateway || b.Status != "error" || b.Error == "" {
		t.Fatalf("code=%d body=%+v", code, b)
	}
}
