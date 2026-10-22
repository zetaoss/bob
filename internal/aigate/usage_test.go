package aigate

import (
	"context"
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

	u.record("gemini/a", nil, nil)
	u.record("gemini/a", nil, &providerCallError{StatusCode: http.StatusTooManyRequests, Err: errors.New("quota"), Body: []byte(`{"error":{"details":[{"violations":[{"quotaId":"PerDay","quotaValue":"20"}]}]}}`)})

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

	// A per-minute 429 cools the model down without emptying the learned daily quota.
	u.record("gemini/a", nil, &providerCallError{StatusCode: http.StatusTooManyRequests, Err: errors.New("rpm"), Body: []byte(`{"error":{"details":[{"violations":[{"quotaId":"PerMinute","quotaValue":"10"}]},{"retryDelay":"30s"}]}}`)})
	got = u.snapshot([]string{"gemini/a"}, routes)[0]
	if got.Available || *got.Remaining != 20 {
		t.Fatalf("expected a per-minute cooldown to keep remaining at 20, got %+v remaining=%d", got, *got.Remaining)
	}
	now = now.Add(time.Minute)

	// Every call except a 429 rejection counts against the learned limit, including other errors.
	u.record("gemini/a", nil, &providerCallError{StatusCode: http.StatusInternalServerError, Err: errors.New("boom")})
	u.record("gemini/a", nil, nil)
	got = u.snapshot([]string{"gemini/a"}, routes)[0]
	if got.Requests != 3 || *got.Remaining != 18 {
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

func TestGroqReportedQuota(t *testing.T) {
	var mu sync.Mutex
	remaining := 3
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer groq-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("X-Ratelimit-Limit-Requests", "1000")
		w.Header().Set("X-Ratelimit-Reset-Requests", "1m26.4s")
		if remaining == 0 {
			w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
			w.Header().Set("Retry-After", "86")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limit","type":"requests"}}`)
			return
		}
		remaining--
		w.Header().Set("X-Ratelimit-Remaining-Requests", strconv.Itoa(remaining))
		_, _ = io.WriteString(w, `{"model":"openai/gpt-oss-20b","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(ts.Close)
	old := groqDefaultEndpoint
	groqDefaultEndpoint = ts.URL
	t.Cleanup(func() { groqDefaultEndpoint = old })

	cfg := &config.AIGateConfig{
		Models:    []string{"groq/openai/gpt-oss-20b"},
		Providers: map[string]config.ProviderConfig{"groq": {APIKey: "groq-key"}},
		Fallback:  config.FallbackConfig{Rounds: 2, PerAttemptTimeout: 5 * time.Second},
	}
	g, err := NewGateway(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	g.usage.now = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, quotaLoc) }

	code, payload := chat(t, g, `{"model":"groq/openai/gpt-oss-20b","messages":[{"role":"user","content":"x"}]}`)
	if code != http.StatusOK || payload["model"] != "groq/openai/gpt-oss-20b" {
		t.Fatalf("unexpected groq response: %d %v", code, payload)
	}
	got := g.usage.snapshot([]string{"groq/openai/gpt-oss-20b"}, g.modelRoutes)[0]
	if got.QuotaSource != "provider" || *got.DailyLimit != 1000 || *got.Remaining != 2 || !got.QuotaReset.Equal(g.usage.now().Add(86400*time.Millisecond)) {
		t.Fatalf("unexpected reported quota: %+v remaining=%d", got, *got.Remaining)
	}

	remaining = 0
	code, _, header := chatWithHeader(t, g, `{"model":"groq/openai/gpt-oss-20b","messages":[{"role":"user","content":"x"}]}`)
	if code != http.StatusBadGateway {
		t.Fatalf("expected the 429 to fail the request, got %d", code)
	}
	code, _, header = chatWithHeader(t, g, `{"model":"groq/openai/gpt-oss-20b","messages":[{"role":"user","content":"x"}]}`)
	if code != http.StatusTooManyRequests || header.Get("Retry-After") != "86" {
		t.Fatalf("expected a cooldown from Retry-After, got %d %q", code, header.Get("Retry-After"))
	}
	got = g.usage.snapshot([]string{"groq/openai/gpt-oss-20b"}, g.modelRoutes)[0]
	if got.Available || *got.Remaining != 0 || got.Requests != 2 {
		t.Fatalf("unexpected usage after 429: %+v remaining=%d", got, *got.Remaining)
	}

	// The quota refills one request per 86.4s (1000 a day), the first at the reported reset.
	start := g.usage.now()
	g.usage.now = func() time.Time { return start.Add(86400*time.Millisecond + 3*86400*time.Millisecond) }
	got = g.usage.snapshot([]string{"groq/openai/gpt-oss-20b"}, g.modelRoutes)[0]
	if *got.Remaining != 4 {
		t.Fatalf("expected 4 requests refilled, got %d", *got.Remaining)
	}
}

func TestFetchGroqModels(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer groq-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"qwen/qwen3.8-27b"},{"id":"openai/gpt-oss-20b"}]}`)
	}))
	t.Cleanup(ts.Close)
	old := groqModelsEndpoint
	groqModelsEndpoint = ts.URL
	t.Cleanup(func() { groqModelsEndpoint = old })

	g := &Gateway{providerMap: map[string]config.ProviderConfig{"groq": {APIKey: "groq-key"}}}
	models, err := g.fetchGroqModels(context.Background())
	if err != nil || strings.Join(models, ",") != "openai/gpt-oss-20b,qwen/qwen3.8-27b" {
		t.Fatalf("unexpected groq models: %v %v", models, err)
	}
}
