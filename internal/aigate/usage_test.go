package aigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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

	// A daily token quota exhausts the model for the day but is not a request count.
	tokensPerDay := `{"error":{"details":[{"violations":[{"quotaMetric":"generativelanguage.googleapis.com/generate_content_free_tier_input_token_count","quotaId":"GenerateContentInputTokensPerModelPerDay-FreeTier","quotaValue":"250000"}]}]}}`
	q = parseQuotaError([]byte(tokensPerDay))
	if !q.daily || q.limit != 0 {
		t.Fatalf("expected a daily cooldown without a request limit, got %+v", q)
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
	u.record("gemini/a", nil, &providerCallError{StatusCode: http.StatusTooManyRequests, Err: errors.New("quota"), Body: []byte(`{"error":{"details":[{"violations":[{"quotaMetric":"generativelanguage.googleapis.com/generate_content_free_tier_requests","quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier","quotaValue":"20"}]}]}}`)})

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
		Fallback:  config.FallbackConfig{MaxFallbacks: intPtr(2), PerAttemptTimeout: 5 * time.Second},
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
	const limit = 1000 // a day, so one request returns every 86.4s
	slot := 86400 * time.Millisecond
	var mu sync.Mutex
	remaining := 10
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer groq-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if remaining > 0 {
			remaining--
		}
		// Groq reports the time until the bucket is full again: one slot per used request.
		w.Header().Set("X-Ratelimit-Limit-Requests", strconv.Itoa(limit))
		w.Header().Set("X-Ratelimit-Remaining-Requests", strconv.Itoa(remaining))
		w.Header().Set("X-Ratelimit-Reset-Requests", (time.Duration(limit-remaining) * slot).String())
		if remaining == 0 {
			w.Header().Set("Retry-After", "86")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limit","type":"requests"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"model":"openai/gpt-oss-20b","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(ts.Close)
	old := groqDefaultEndpoint
	groqDefaultEndpoint = ts.URL
	t.Cleanup(func() { groqDefaultEndpoint = old })

	cfg := &config.AIGateConfig{
		Models:    []string{"groq/openai/gpt-oss-20b"},
		Providers: map[string]config.ProviderConfig{"groq": {APIKey: "groq-key"}},
		Fallback:  config.FallbackConfig{MaxFallbacks: intPtr(2), PerAttemptTimeout: 5 * time.Second},
	}
	g, err := NewGateway(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, quotaLoc)
	g.usage.now = func() time.Time { return start }
	snapshot := func() ModelUsage { return g.usage.snapshot([]string{"groq/openai/gpt-oss-20b"}, g.modelRoutes)[0] }
	body := `{"model":"groq/openai/gpt-oss-20b","messages":[{"role":"user","content":"x"}]}`

	if code, payload := chat(t, g, body); code != http.StatusOK || payload["model"] != "groq/openai/gpt-oss-20b" {
		t.Fatalf("unexpected groq response: %d %v", code, payload)
	}
	got := snapshot()
	if got.QuotaSource != "provider" || *got.DailyLimit != limit || *got.Remaining != 9 || !got.QuotaReset.Equal(start.Add(991*slot)) {
		t.Fatalf("unexpected reported quota: %+v remaining=%d", got, *got.Remaining)
	}

	mu.Lock()
	remaining = 0
	mu.Unlock()
	if code, _ := chat(t, g, body); code != http.StatusBadGateway {
		t.Fatalf("expected the 429 to fail the request, got %d", code)
	}
	if code, _, header := chatWithHeader(t, g, body); code != http.StatusTooManyRequests || header.Get("Retry-After") != "86" {
		t.Fatalf("expected a cooldown from Retry-After, got %d %q", code, header.Get("Retry-After"))
	}
	if got = snapshot(); got.Available || *got.Remaining != 0 || got.Requests != 2 {
		t.Fatalf("unexpected usage after 429: %+v remaining=%d", got, *got.Remaining)
	}

	// Refilling starts at the report, one request per slot, until the bucket is full at the reset.
	g.usage.now = func() time.Time { return start.Add(4*slot + time.Second) }
	if got = snapshot(); *got.Remaining != 4 {
		t.Fatalf("expected 4 requests refilled, got %d", *got.Remaining)
	}
	g.usage.now = func() time.Time { return start.Add(limit * slot) }
	if got = snapshot(); *got.Remaining != limit {
		t.Fatalf("expected a full bucket at the reset, got %d", *got.Remaining)
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

func TestCloudflareWorkersAI(t *testing.T) {
	var gotPath, gotModel string
	pages := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cf-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/accounts/acct/ai/models/search":
			if r.URL.Query().Get("task") != "Text Generation" {
				t.Errorf("unexpected task filter %q", r.URL.Query().Get("task"))
			}
			pages++
			// A full first page, then a short one, to exercise paging.
			if r.URL.Query().Get("page") == "1" {
				var items []string
				for i := range 100 {
					items = append(items, fmt.Sprintf(`{"name":"@cf/x/model-%03d"}`, i))
				}
				_, _ = io.WriteString(w, `{"success":true,"result":[`+strings.Join(items, ",")+`]}`)
				return
			}
			_, _ = io.WriteString(w, `{"success":true,"result":[{"name":"@cf/openai/gpt-oss-120b"}]}`)
		case "/accounts/acct/ai/v1/chat/completions":
			var req struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotPath, gotModel = r.URL.Path, req.Model
			_, _ = io.WriteString(w, `{"object":"chat.completion","model":"@cf/openai/gpt-oss-120b","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"neurons":0.1}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	old := cloudflareAPIBase
	cloudflareAPIBase = ts.URL
	t.Cleanup(func() { cloudflareAPIBase = old })

	cfg := &config.AIGateConfig{
		Models:    []string{"cloudflare/@cf/openai/gpt-oss-120b"},
		Providers: map[string]config.ProviderConfig{"cloudflare": {APIKey: "cf-token", AccountID: "acct"}},
		Fallback:  config.FallbackConfig{MaxFallbacks: intPtr(2), PerAttemptTimeout: 5 * time.Second},
	}
	g, err := NewGateway(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	models, err := g.fetchCloudflareModels(context.Background())
	if err != nil || len(models) != 101 || pages != 2 || !slices.Contains(models, "@cf/openai/gpt-oss-120b") {
		t.Fatalf("unexpected cloudflare models: %d models, %d pages, %v", len(models), pages, err)
	}

	code, payload := chat(t, g, `{"model":"cloudflare/@cf/openai/gpt-oss-120b","messages":[{"role":"user","content":"x"}]}`)
	if code != http.StatusOK || payload["model"] != "cloudflare/@cf/openai/gpt-oss-120b" {
		t.Fatalf("unexpected response: %d %v", code, payload)
	}
	if gotPath != "/accounts/acct/ai/v1/chat/completions" || gotModel != "@cf/openai/gpt-oss-120b" {
		t.Fatalf("unexpected upstream request: %s %s", gotPath, gotModel)
	}
}

func intPtr(v int) *int { return &v }

func TestChatMaxFallbacks(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		calls[req.Model]++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	old := geminiDefaultEndpoint
	geminiDefaultEndpoint = ts.URL
	t.Cleanup(func() { geminiDefaultEndpoint = old })

	cfg := &config.AIGateConfig{
		Models:    []string{"gemini/a", "gemini/b", "gemini/c", "gemini/d", "gemini/e"},
		Providers: map[string]config.ProviderConfig{"gemini": {}},
		Fallback:  config.FallbackConfig{MaxFallbacks: intPtr(2), PerAttemptTimeout: 5 * time.Second},
	}
	g, err := NewGateway(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// gemini/b is cooling down, so it is skipped and not counted: a, c, d are called, e is not.
	g.usage.models["gemini/b"] = &modelUsage{cooldownUntil: time.Now().Add(time.Hour)}

	if code, _ := chat(t, g, `{"messages":[{"role":"user","content":"x"}]}`); code != http.StatusBadGateway {
		t.Fatalf("expected all calls to fail, got %d", code)
	}
	if calls["a"] != 1 || calls["b"] != 0 || calls["c"] != 1 || calls["d"] != 1 || calls["e"] != 0 {
		t.Fatalf("expected one call each to a, c, d, got %v", calls)
	}
}

func TestUnavailableCooldown(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, quotaLoc)
	u := newUsageTracker(func() time.Time { return now })
	u.record("gemini/a", nil, &providerCallError{StatusCode: http.StatusServiceUnavailable, Err: errors.New("high demand")})
	if until, cooling := u.coolingDown("gemini/a"); !cooling || !until.Equal(now.Add(unavailableCooldown)) {
		t.Fatalf("expected a %s cooldown after 503, got %v %v", unavailableCooldown, until, cooling)
	}
	now = now.Add(unavailableCooldown)
	if _, cooling := u.coolingDown("gemini/a"); cooling {
		t.Fatal("expected the cooldown to end")
	}
	// A 500 is not a cooldown: it may be about the request, not the model.
	u.record("gemini/a", nil, &providerCallError{StatusCode: http.StatusInternalServerError, Err: errors.New("boom")})
	if _, cooling := u.coolingDown("gemini/a"); cooling {
		t.Fatal("expected no cooldown after 500")
	}
}

func TestFallbackChainInterleavesProviders(t *testing.T) {
	cfg := &config.AIGateConfig{
		Models: []string{"gemini/g1", "gemini/g2", "gemini/g3", "groq/q1", "cloudflare/c1", "groq/q2"},
		Providers: map[string]config.ProviderConfig{
			"gemini": {}, "groq": {}, "cloudflare": {AccountID: "a"},
		},
	}
	g, err := NewGateway(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		model    string
		fallback bool
		want     string
	}{
		{"", false, "gemini/g1,groq/q1,cloudflare/c1,gemini/g2,groq/q2,gemini/g3"},
		{"groq/q1", true, "groq/q1,gemini/g1,cloudflare/c1,groq/q2,gemini/g2,gemini/g3"},
		{"groq/q1", false, "groq/q1"},
		{"gemini", false, "gemini/g1,gemini/g2,gemini/g3"},
	}
	for _, c := range cases {
		chain, bad := g.resolveModelChain(c.model, c.fallback)
		if bad || strings.Join(chain, ",") != c.want {
			t.Errorf("model %q fallback %v: got %v, want %s", c.model, c.fallback, chain, c.want)
		}
	}
}
