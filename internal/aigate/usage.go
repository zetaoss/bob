package aigate

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Gemini daily quotas reset at midnight Pacific time.
var quotaLoc, _ = time.LoadLocation("America/Los_Angeles")

// usageTracker counts provider calls per model for the current quota day and remembers quota
// limits and cooldowns learned from 429 responses or reported in response headers (Groq).
// State is in memory only.
type usageTracker struct {
	mu     sync.Mutex
	now    func() time.Time
	day    string
	models map[string]*modelUsage
}

type modelUsage struct {
	requests      int
	successes     int
	failures      int
	rejected      int
	dailyLimit    int
	cooldownUntil time.Time
	lastError     string
	// Set when the provider reports its request quota in response headers.
	reportedRemaining int
	reportedReset     time.Time
	reported          bool
}

type ModelUsage struct {
	ID            string     `json:"id"`
	Provider      string     `json:"provider"`
	Requests      int        `json:"requests"`
	Successes     int        `json:"successes"`
	Failures      int        `json:"failures"`
	DailyLimit    *int       `json:"daily_limit"`
	Remaining     *int       `json:"remaining"`
	Available     bool       `json:"available"`
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	// QuotaSource is "provider" when daily_limit and remaining come from provider headers,
	// "estimate" when learned from a 429 and counted by bob.
	QuotaSource string     `json:"quota_source,omitempty"`
	QuotaReset  *time.Time `json:"quota_reset,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

func newUsageTracker(now func() time.Time) *usageTracker {
	return &usageTracker{now: now, models: make(map[string]*modelUsage)}
}

func quotaDay(t time.Time) string {
	return t.In(quotaLoc).Format(time.DateOnly)
}

func nextQuotaReset(t time.Time) time.Time {
	local := t.In(quotaLoc)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, quotaLoc)
}

// entry returns the model's usage, resetting counters when the quota day has changed. Callers hold mu.
func (u *usageTracker) entry(model string) *modelUsage {
	if day := quotaDay(u.now()); day != u.day {
		u.day = day
		for _, m := range u.models {
			m.requests, m.successes, m.failures, m.rejected = 0, 0, 0, 0
		}
	}
	m, ok := u.models[model]
	if !ok {
		m = &modelUsage{}
		u.models[model] = m
	}
	return m
}

// coolingDown reports whether the model is in a cooldown from an earlier quota error.
func (u *usageTracker) coolingDown(model string) (time.Time, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	m := u.entry(model)
	if u.now().Before(m.cooldownUntil) {
		return m.cooldownUntil, true
	}
	return time.Time{}, false
}

func (u *usageTracker) record(model string, header http.Header, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	m := u.entry(model)
	m.requests++
	var callErr *providerCallError
	if header == nil && errors.As(err, &callErr) {
		header = callErr.Header
	}
	if limit, remaining, reset, ok := parseRateLimitHeaders(header); ok {
		m.dailyLimit, m.reportedRemaining, m.reported = limit, remaining, true
		m.reportedReset = u.now().Add(reset)
	}
	if err == nil {
		m.successes++
		return
	}
	m.failures++
	m.lastError = truncate(err.Error(), 300)

	if !errors.As(err, &callErr) || callErr.StatusCode != http.StatusTooManyRequests {
		return
	}
	m.rejected++
	q := parseQuotaError(callErr.Body)
	if q.daily && q.limit > 0 && !m.reported {
		m.dailyLimit = q.limit
	}
	retryAfter, _ := strconv.Atoi(callErr.Header.Get("Retry-After"))
	switch {
	case q.retryDelay > 0:
		m.cooldownUntil = u.now().Add(q.retryDelay)
	case retryAfter > 0:
		m.cooldownUntil = u.now().Add(time.Duration(retryAfter) * time.Second)
	case q.daily:
		m.cooldownUntil = nextQuotaReset(u.now())
	}
}

// parseRateLimitHeaders reads the request quota Groq reports on every response:
// x-ratelimit-limit-requests and x-ratelimit-remaining-requests (per day), and
// x-ratelimit-reset-requests (a duration such as "1m26.4s" until the next request is restored;
// the quota refills gradually, limit requests per day, rather than all at once).
func parseRateLimitHeaders(h http.Header) (limit, remaining int, reset time.Duration, ok bool) {
	if h == nil {
		return 0, 0, 0, false
	}
	limit, err1 := strconv.Atoi(h.Get("X-Ratelimit-Limit-Requests"))
	remaining, err2 := strconv.Atoi(h.Get("X-Ratelimit-Remaining-Requests"))
	if err1 != nil || err2 != nil || limit <= 0 {
		return 0, 0, 0, false
	}
	reset, _ = time.ParseDuration(h.Get("X-Ratelimit-Reset-Requests"))
	return limit, remaining, reset, true
}

func (u *usageTracker) snapshot(models []string, routes map[string]ModelRoute) []ModelUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	out := make([]ModelUsage, 0, len(models))
	for _, id := range models {
		m := u.entry(id)
		item := ModelUsage{
			ID:        id,
			Provider:  routes[id].Provider,
			Requests:  m.requests,
			Successes: m.successes,
			Failures:  m.failures,
			Available: !now.Before(m.cooldownUntil),
			LastError: m.lastError,
		}
		switch {
		case m.reported:
			limit, remaining := m.dailyLimit, m.reportedRemaining
			// Add the requests refilled since the report: the first at reportedReset, then one per day/limit.
			if remaining < limit && !now.Before(m.reportedReset) {
				interval := 24 * time.Hour / time.Duration(limit)
				remaining = min(limit, remaining+1+int(now.Sub(m.reportedReset)/interval))
			}
			reset := m.reportedReset
			if now.After(reset) {
				reset = now
			}
			item.DailyLimit, item.Remaining, item.QuotaSource, item.QuotaReset = &limit, &remaining, "provider", &reset
		case m.dailyLimit > 0:
			limit := m.dailyLimit
			// Every call except a 429 rejection may count against the quota, including other errors and timeouts.
			remaining := max(limit-(m.requests-m.rejected), 0)
			if !item.Available {
				remaining = 0
			}
			reset := nextQuotaReset(now)
			item.DailyLimit, item.Remaining, item.QuotaSource, item.QuotaReset = &limit, &remaining, "estimate", &reset
		}
		if !item.Available {
			until := m.cooldownUntil
			item.CooldownUntil = &until
		}
		out = append(out, item)
	}
	return out
}

type quotaError struct {
	daily      bool
	limit      int
	retryDelay time.Duration
}

// parseQuotaError reads a Gemini 429 body: an error object, or a list of them.
func parseQuotaError(body []byte) quotaError {
	type geminiError struct {
		Error struct {
			Details []struct {
				Type       string `json:"@type"`
				Violations []struct {
					QuotaID    string `json:"quotaId"`
					QuotaValue string `json:"quotaValue"`
				} `json:"violations"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	var list []geminiError
	if err := json.Unmarshal(body, &list); err != nil {
		var one geminiError
		if json.Unmarshal(body, &one) != nil {
			return quotaError{}
		}
		list = []geminiError{one}
	}

	var q quotaError
	for _, e := range list {
		for _, d := range e.Error.Details {
			if d.RetryDelay != "" {
				if delay, err := time.ParseDuration(d.RetryDelay); err == nil {
					q.retryDelay = max(q.retryDelay, delay)
				}
			}
			for _, v := range d.Violations {
				if !strings.Contains(v.QuotaID, "PerDay") {
					continue
				}
				q.daily = true
				if n, err := strconv.Atoi(v.QuotaValue); err == nil {
					q.limit = n
				}
			}
		}
	}
	return q
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (g *Gateway) UsageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	now := g.usage.now()
	writeJSON(w, http.StatusOK, map[string]any{
		"quota_day":  quotaDay(now),
		"next_reset": nextQuotaReset(now),
		"data":       g.usage.snapshot(slices.Clone(g.modelOrder), g.modelRoutes),
	})
}
