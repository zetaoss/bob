// Package search counts results for 2-4 queries across search engines (moved from zetaoss/queryhub).
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"bob/internal/config"
)

// Engine is one search API; Total reads the result count from its JSON response.
type Engine struct {
	Name       string
	BaseURL    string
	Headers    map[string]string
	Params     map[string]string
	QueryParam string
	// ExactMatch wraps the query in double quotes.
	ExactMatch bool
	TotalField string
}

// Engines returns the engines whose credentials are configured, sorted by name.
func Engines(cfg config.SearchConfig) []Engine {
	var engines []Engine
	if cfg.KakaoAPIKey != "" {
		kakao := map[string]string{"Authorization": "KakaoAK " + cfg.KakaoAPIKey}
		engines = append(engines,
			Engine{Name: "daum_blog", BaseURL: "https://dapi.kakao.com/v2/search/blog", Headers: kakao, QueryParam: "query", TotalField: "meta.total_count"},
		)
	}
	if cfg.NaverClientID != "" && cfg.NaverClientSecret != "" {
		naver := map[string]string{"X-Naver-Client-Id": cfg.NaverClientID, "X-Naver-Client-Secret": cfg.NaverClientSecret}
		engines = append(engines,
			Engine{Name: "naver_blog", BaseURL: "https://openapi.naver.com/v1/search/blog.json", Headers: naver, QueryParam: "query", TotalField: "total"},
			Engine{Name: "naver_news", BaseURL: "https://openapi.naver.com/v1/search/news.json", Headers: naver, QueryParam: "query", TotalField: "total"},
		)
	}
	if cfg.GoogleAPIKey != "" && cfg.GoogleCX != "" {
		engines = append(engines,
			Engine{Name: "google_search", BaseURL: "https://www.googleapis.com/customsearch/v1", Params: map[string]string{"key": cfg.GoogleAPIKey, "cx": cfg.GoogleCX}, QueryParam: "q", ExactMatch: true, TotalField: "searchInformation.totalResults"},
		)
	}
	slices.SortFunc(engines, func(a, b Engine) int { return strings.Compare(a.Name, b.Name) })
	return engines
}

type Handler struct {
	engines []Engine
	client  *http.Client
	log     *slog.Logger
}

func NewHandler(engines []Engine, log *slog.Logger) *Handler {
	return &Handler{engines: engines, client: &http.Client{Timeout: 10 * time.Second}, log: log}
}

type response struct {
	Status string `json:"status"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ServeHTTP answers GET /search?q=a&q=b[&q=c&q=d] with
// {"status":"ok","result":{"engines":[...],"values":[[count per engine] per query]}}.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/search" {
		writeJSON(w, http.StatusNotFound, response{Status: "error", Error: "not found"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, response{Status: "error", Error: "method not allowed"})
		return
	}
	queries := r.URL.Query()["q"]
	if len(queries) < 2 || len(queries) > 4 {
		writeJSON(w, http.StatusBadRequest, response{Status: "error", Error: "the number of search queries must be between 2 and 4"})
		return
	}

	values, err := h.count(r.Context(), queries)
	if err != nil {
		h.log.Error("search failed", "queries", queries, "err", err)
		writeJSON(w, http.StatusInternalServerError, response{Status: "error", Error: err.Error()})
		return
	}
	names := make([]string, len(h.engines))
	for i, e := range h.engines {
		names[i] = e.Name
	}
	writeJSON(w, http.StatusOK, response{Status: "ok", Result: map[string]any{"engines": names, "values": values}})
}

// count queries every engine for every query in parallel; any failure fails the request.
func (h *Handler) count(ctx context.Context, queries []string) ([][]int, error) {
	values := make([][]int, len(queries))
	errs := make([][]error, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		values[i] = make([]int, len(h.engines))
		errs[i] = make([]error, len(h.engines))
		for j, e := range h.engines {
			wg.Go(func() {
				values[i][j], errs[i][j] = h.fetch(ctx, e, q)
			})
		}
	}
	wg.Wait()
	for i := range queries {
		for j, e := range h.engines {
			if errs[i][j] != nil {
				return nil, fmt.Errorf("%s error: %w", e.Name, errs[i][j])
			}
		}
	}
	return values, nil
}

func (h *Handler) fetch(ctx context.Context, e Engine, query string) (int, error) {
	if e.ExactMatch {
		query = `"` + query + `"`
	}
	params := url.Values{e.QueryParam: []string{query}}
	for k, v := range e.Params {
		params.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.BaseURL+"?"+params.Encode(), nil)
	if err != nil {
		return 0, err
	}
	for k, v := range e.Headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		// The URL may carry an API key, so only the base URL is reported.
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, e.BaseURL)
	}
	return parseTotal(body, e.TotalField)
}

func parseTotal(body []byte, field string) (int, error) {
	var current any
	if err := json.Unmarshal(body, &current); err != nil {
		return 0, err
	}
	for _, key := range strings.Split(field, ".") {
		m, ok := current.(map[string]any)
		if !ok {
			return 0, fmt.Errorf("key %s not found", key)
		}
		current = m[key]
	}
	switch v := current.(type) {
	case float64:
		return int(v), nil
	case string:
		return strconv.Atoi(v)
	default:
		return 0, fmt.Errorf("unexpected %s: %#v", field, current)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
