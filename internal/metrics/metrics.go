// Package metrics runs named PromQL queries from the config against a Prometheus-compatible API.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"bob/internal/config"
)

// Sample is one series of an instant query result.
type Sample struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

type Handler struct {
	prometheus string
	queries    map[string]string
	client     *http.Client
	log        *slog.Logger
}

func NewHandler(cfg config.MetricsConfig, log *slog.Logger) *Handler {
	return &Handler{
		prometheus: strings.TrimRight(cfg.Prometheus, "/"),
		queries:    cfg.Queries,
		client:     &http.Client{Timeout: 10 * time.Second},
		log:        log,
	}
}

type response struct {
	Status string              `json:"status"`
	Time   string              `json:"time,omitempty"`
	Result map[string][]Sample `json:"result,omitempty"`
	Error  string              `json:"error,omitempty"`
}

// ServeHTTP answers GET /[?name=<name>...][&time=<RFC3339>] with the named metrics (all when no
// name is given) and GET /<name> with one. An unknown name is a 404. Without time, the current
// values are returned. Any failed query fails the request (502).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, response{Status: "error", Error: "method not allowed"})
		return
	}
	names := r.URL.Query()["name"]
	if name := strings.TrimPrefix(r.URL.Path, "/"); name != "" {
		names = []string{name}
	}
	if len(names) == 0 {
		for name := range h.queries {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	var unknown []string
	for _, name := range names {
		if _, ok := h.queries[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		writeJSON(w, http.StatusNotFound, response{Status: "error", Error: "unknown metric: " + strings.Join(unknown, ", ")})
		return
	}

	at := time.Now().UTC()
	if v := r.URL.Query().Get("time"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, response{Status: "error", Error: "time must be RFC3339"})
			return
		}
		at = t.UTC()
	}

	result, err := h.Query(r.Context(), names, at)
	if err != nil {
		h.log.Error("metrics query failed", "err", err)
		writeJSON(w, http.StatusBadGateway, response{Status: "error", Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, response{Status: "ok", Time: at.Format(time.RFC3339), Result: result})
}

// Query runs the named queries in parallel at the given time.
func (h *Handler) Query(ctx context.Context, names []string, at time.Time) (map[string][]Sample, error) {
	results := make([][]Sample, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			results[i], errs[i] = h.instant(ctx, h.queries[name], at)
		})
	}
	wg.Wait()

	out := make(map[string][]Sample, len(names))
	for i, name := range names {
		if errs[i] != nil {
			return nil, fmt.Errorf("%s: %w", name, errs[i])
		}
		out[name] = results[i]
	}
	return out, nil
}

func (h *Handler) instant(ctx context.Context, query string, at time.Time) ([]Sample, error) {
	params := url.Values{"query": []string{query}, "time": []string{at.Format(time.RFC3339)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.prometheus+"/api/v1/query?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus status code %d", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus status %s: %s", body.Status, body.Error)
	}
	return parse(body.Data.ResultType, body.Data.Result)
}

// parse reads vector and scalar results. Samples whose value is NaN or infinite are dropped.
func parse(resultType string, raw json.RawMessage) ([]Sample, error) {
	samples := []Sample{}
	add := func(labels map[string]string, value []any) error {
		if len(value) < 2 {
			return fmt.Errorf("malformed sample value")
		}
		s, ok := value[1].(string)
		if !ok {
			return fmt.Errorf("malformed sample value %v", value[1])
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		if labels == nil {
			labels = map[string]string{}
		}
		samples = append(samples, Sample{Labels: labels, Value: v})
		return nil
	}
	switch resultType {
	case "vector":
		var vector []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		}
		if err := json.Unmarshal(raw, &vector); err != nil {
			return nil, err
		}
		for _, s := range vector {
			if err := add(s.Metric, s.Value); err != nil {
				return nil, err
			}
		}
	case "scalar":
		var value []any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		if err := add(nil, value); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported result type %q (use an instant vector or scalar query)", resultType)
	}
	return samples, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
