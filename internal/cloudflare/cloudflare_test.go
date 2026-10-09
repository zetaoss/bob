package cloudflare

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bob/internal/config"
)

type call struct {
	auth      string
	variables map[string]any
	query     string
}

func fakeCloudflare(t *testing.T, body string, calls *[]call) *Handler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		*calls = append(*calls, call{auth: r.Header.Get("Authorization"), variables: req.Variables, query: req.Query})
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	h := NewHandler(config.CloudflareConfig{APIToken: "tok", ZoneID: "zone1"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.graphqlURL = srv.URL
	return h
}

const sample = `{"data":{"viewer":{"zones":[{"zones":[{
 "dimensions":{"timeslot":"2026-10-09T10:00:00Z"},
 "uniq":{"uniques":1234},
 "sum":{"requests":2345678,"pageViews":12,"bytes":1.5e9,"cachedBytes":0,"cachedRequests":3,"encryptedBytes":4,"encryptedRequests":5,"threats":0,
  "browserMap":[{"key":"Chrome","pageViews":7}],"contentTypeMap":[],"clientSSLMap":[{"key":"TLSv1.3","requests":9}],
  "countryMap":[{"bytes":1,"key":"KR","requests":2,"threats":0}],"ipClassMap":[],"responseStatusMap":[{"key":200,"requests":8}],"threatPathingMap":[]}}]}]}}}`

func TestServeHTTP_Hourly(t *testing.T) {
	var calls []call
	h := fakeCloudflare(t, sample, &calls)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/analytics?interval=hour&since=2026-10-07T11:00:00Z&until=2026-10-09T11:00:00Z", nil))

	var body struct {
		Status string  `json:"status"`
		Result []Group `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(calls) != 2 {
		t.Fatalf("48h should be split into two 24h windows, got %d calls", len(calls))
	}
	if calls[0].auth != "Bearer tok" || calls[0].variables["zoneTag"] != "zone1" || calls[1].variables["since"] != "2026-10-08T11:00:00Z" || !strings.Contains(calls[0].query, "httpRequests1hGroups") {
		t.Errorf("calls=%+v", calls)
	}
	m := body.Result[0].Metrics
	want := map[string]string{
		"uniq_uniques":          "1234",
		"sum_requests":          "2.345678e+06",
		"sum_bytes":             "1.5e+09",
		"sum_cachedBytes":       "0",
		"sum_browserMap":        `[{"key":"Chrome","pageViews":7}]`,
		"sum_contentTypeMap":    `[]`,
		"sum_countryMap":        `[{"bytes":1,"key":"KR","requests":2,"threats":0}]`,
		"sum_responseStatusMap": `[{"key":200,"requests":8}]`,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if len(m) != len(MetricNames) || body.Result[0].Timeslot != "2026-10-09T10:00:00Z" {
		t.Errorf("metrics=%d timeslot=%s", len(m), body.Result[0].Timeslot)
	}
}

func TestServeHTTP_Daily(t *testing.T) {
	var calls []call
	h := fakeCloudflare(t, `{"data":{"viewer":{"zones":[{"zones":[{"dimensions":{"timeslot":"2026-10-08"},"uniq":{"uniques":1}}]}]}}}`, &calls)

	got, err := h.Daily(t.Context(), time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].variables["since"] != "2026-09-30" || calls[0].variables["until"] != "2026-10-10" || !strings.Contains(calls[0].query, "httpRequests1dGroups") {
		t.Errorf("calls=%+v", calls)
	}
	if len(got) != 1 || got[0].Timeslot != "2026-10-08" || got[0].Metrics["uniq_uniques"] != "1" || len(got[0].Metrics) != 1 {
		t.Errorf("groups=%+v", got)
	}
}

func TestServeHTTP_Errors(t *testing.T) {
	var calls []call
	h := fakeCloudflare(t, `{"errors":[{"message":"bad token"}],"data":null}`, &calls)
	for target, want := range map[string]int{
		"/analytics?interval=week":                                  http.StatusBadRequest,
		"/analytics?interval=hour&since=2026-10-09&until=x":         http.StatusBadRequest,
		"/analytics?interval=day&since=2026-10-09&until=2026-10-01": http.StatusBadRequest,
		"/other": http.StatusNotFound,
		"/analytics?interval=day&since=2026-10-01&until=2026-10-09": http.StatusBadGateway,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != want {
			t.Errorf("%s: code=%d, want %d", target, rec.Code, want)
		}
	}
}
