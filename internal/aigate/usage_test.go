package aigate

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bob/internal/config"
)

// Shape of a Gemini free-tier daily quota error, trimmed.
const dailyQuotaBody = `[{
  "error": {
    "code": 429,
    "status": "RESOURCE_EXHAUSTED",
    "details": [
      {"@type": "type.googleapis.com/google.rpc.QuotaFailure",
       "violations": [{"quotaMetric": "generativelanguage.googleapis.com/generate_content_free_tier_requests",
                       "quotaId": "GenerateRequestsPerDayPerProjectPerModel-FreeTier", "quotaValue": "20"}]},
      {"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "55475s"}
    ]
  }
}]`

func TestParseQuotaError(t *testing.T) {
	q := parseQuotaError([]byte(dailyQuotaBody))
	if !q.daily || q.limit != 20 || q.retryDelay != 55475*time.Second {
		t.Fatalf("unexpected quota error: %+v", q)
	}

	perMinute := `{"error":{"details":[{"violations":[{"quotaId":"GenerateRequestsPerMinutePerProjectPerModel-FreeTier","quotaValue":"10"}]},{"retryDelay":"30s"}]}}`
	q = parseQuotaError([]byte(perMinute))
	if q.daily || q.limit != 0 || q.retryDelay != 30*time.Second {
		t.Fatalf("unexpected per-minute quota error: %+v", q)
	}

	if q := parseQuotaError([]byte("not json")); q.daily || q.retryDelay != 0 {
		t.Fatalf("expected empty quota error, got %+v", q)
	}
}

func TestUsageTrackerQuotaDay(t *testing.T) {
	now := time.Date(2026, 10, 22, 23, 0, 0, 0, quotaLoc)
	u := newUsageTracker(func() time.Time { return now })
	routes := map[string]ModelRoute{"gemini/a": {Provider: "gemini"}}

	u.record("gemini/a", nil)
	u.record("gemini/a", &providerCallError{StatusCode: http.StatusTooManyRequests, Err: errors.New("quota"), Body: []byte(`{"error":{"details":[{"violations":[{"quotaId":"PerDay","quotaValue":"20"}]}]}}`)})

	got := u.snapshot([]string{"gemini/a"}, routes)[0]
	if got.Requests != 2 || got.Successes != 1 || got.Failures != 1 || got.Available {
		t.Fatalf("unexpected usage: %+v", got)
	}
	if *got.DailyLimit != 20 || *got.Remaining != 0 || !got.CooldownUntil.Equal(time.Date(2026, 10, 23, 0, 0, 0, 0, quotaLoc)) {
		t.Fatalf("unexpected quota: limit=%d remaining=%d until=%v", *got.DailyLimit, *got.Remaining, got.CooldownUntil)
	}

	now = now.Add(2 * time.Hour)
	got = u.snapshot([]string{"gemini/a"}, routes)[0]
	if got.Requests != 0 || !got.Available || *got.DailyLimit != 20 || *got.Remaining != 20 {
		t.Fatalf("expected counters reset on a new quota day, got %+v", got)
	}

	// Every call except a 429 rejection counts against the learned limit, including other errors.
	u.record("gemini/a", &providerCallError{StatusCode: http.StatusInternalServerError, Err: errors.New("boom")})
	u.record("gemini/a", nil)
	got = u.snapshot([]string{"gemini/a"}, routes)[0]
	if got.Requests != 2 || *got.Remaining != 18 {
		t.Fatalf("expected the 500 and the success counted, got %+v remaining=%d", got, *got.Remaining)
	}
}

type fakeGemini struct {
	mu    sync.Mutex
	calls map[string]int
	fail  map[string]bool
}

func (f *fakeGemini) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.calls[req.Model]++
	fail := f.fail[req.Model]
	f.mu.Unlock()
	if fail {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, dailyQuotaBody)
		return
	}
	_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
}

func newTestGateway(t *testing.T, fake *fakeGemini) *Gateway {
	t.Helper()
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	old := geminiDefaultEndpoint
	geminiDefaultEndpoint = ts.URL
	t.Cleanup(func() { geminiDefaultEndpoint = old })

	cfg := &config.AIGateConfig{
		Models:    []string{"gemini/a", "gemini/b"},
		Providers: map[string]config.ProviderConfig{"gemini": {}},
		Fallback:  config.FallbackConfig{Rounds: 2, PerAttemptTimeout: 5 * time.Second},
	}
	g, err := NewGateway(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func chat(t *testing.T, g *Gateway, body string) (int, map[string]any) {
	t.Helper()
	code, payload, _ := chatWithHeader(t, g, body)
	return code, payload
}

func chatWithHeader(t *testing.T, g *Gateway, body string) (int, map[string]any, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	return rec.Code, payload, rec.Header()
}

func TestChatSkipsModelsCoolingDown(t *testing.T) {
	fake := &fakeGemini{calls: map[string]int{}, fail: map[string]bool{"a": true}}
	g := newTestGateway(t, fake)

	code, payload := chat(t, g, `{"model":"gemini/a","fallback":true,"messages":[{"role":"user","content":"x"}]}`)
	if code != http.StatusOK || payload["model"] != "gemini/b" {
		t.Fatalf("expected fallback to gemini/b, got %d %v", code, payload)
	}
	code, _ = chat(t, g, `{"model":"gemini/a","fallback":true,"messages":[{"role":"user","content":"x"}]}`)
	if code != http.StatusOK || fake.calls["a"] != 1 || fake.calls["b"] != 2 {
		t.Fatalf("expected gemini/a skipped while cooling down, got %d calls=%v", code, fake.calls)
	}

	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/usage", nil))
	var usage struct {
		Data []ModelUsage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	a, b := usage.Data[0], usage.Data[1]
	if a.ID != "gemini/a" || a.Available || a.Failures != 1 || *a.DailyLimit != 20 || *a.Remaining != 0 {
		t.Fatalf("unexpected usage for gemini/a: %+v", a)
	}
	if b.ID != "gemini/b" || !b.Available || b.Successes != 2 || b.DailyLimit != nil {
		t.Fatalf("unexpected usage for gemini/b: %+v", b)
	}
}

func TestChatConfiguredModelDoesNotFallBack(t *testing.T) {
	fake := &fakeGemini{calls: map[string]int{}, fail: map[string]bool{"a": true}}
	g := newTestGateway(t, fake)

	code, _ := chat(t, g, `{"model":"gemini/a","messages":[{"role":"user","content":"x"}]}`)
	if code != http.StatusBadGateway || fake.calls["b"] != 0 {
		t.Fatalf("expected failure without fallback, got %d calls=%v", code, fake.calls)
	}
	code, payload, header := chatWithHeader(t, g, `{"model":"gemini/a","messages":[{"role":"user","content":"x"}]}`)
	if code != http.StatusTooManyRequests || payload["retry_after"] == nil || fake.calls["a"] != 1 {
		t.Fatalf("expected 429 while cooling down, got %d %v calls=%v", code, payload, fake.calls)
	}
	if secs, err := strconv.Atoi(header.Get("Retry-After")); err != nil || secs < 55000 {
		t.Fatalf("expected Retry-After in seconds until the cooldown ends, got %q", header.Get("Retry-After"))
	}

	code, payload = chat(t, g, `{"model":"gemini","messages":[{"role":"user","content":"x"}]}`)
	if code != http.StatusOK || payload["model"] != "gemini/b" {
		t.Fatalf("expected a provider name to use its models in order, got %d %v", code, payload)
	}
	code, payload = chat(t, g, `{"messages":[{"role":"user","content":"x"}]}`)
	if code != http.StatusOK || payload["model"] != "gemini/b" {
		t.Fatalf("expected an omitted model to use every model in order, got %d %v", code, payload)
	}
}
