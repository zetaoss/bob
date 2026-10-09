package google

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bob/internal/config"
)

const (
	gaScope  = "https://www.googleapis.com/auth/analytics.readonly"
	gscScope = "https://www.googleapis.com/auth/webmasters.readonly"
)

// GARow is one GA4 timeslot: RFC3339 UTC for hour, YYYY-MM-DD for day.
type GARow struct {
	Timeslot        string `json:"timeslot"`
	Sessions        int    `json:"sessions"`
	ScreenPageViews int    `json:"screen_page_views"`
	ActiveUsers     int    `json:"active_users"`
}

// GSCRow is one Search Console timeslot: RFC3339 UTC for hour, YYYY-MM-DD for day. CTR is a
// percentage; CTR and position are rounded to 4 decimals.
type GSCRow struct {
	Timeslot    string  `json:"timeslot"`
	Clicks      int     `json:"clicks"`
	Impressions int     `json:"impressions"`
	CTR         float64 `json:"ctr"`
	Position    float64 `json:"position"`
}

type Handler struct {
	tokens     *tokenSource
	propertyID string
	gaLoc      *time.Location
	siteURL    string
	gaURL      string
	gscURL     string
	client     *http.Client
	log        *slog.Logger
}

// gscLoc is the time zone Search Console reports hours in.
var gscLoc, _ = time.LoadLocation("America/Los_Angeles")

func NewHandler(cfg config.GoogleConfig, log *slog.Logger) (*Handler, error) {
	sa, key, err := parseServiceAccount(cfg.ServiceAccount)
	if err != nil {
		return nil, err
	}
	tz := cfg.GATimezone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("google.gaTimezone: %w", err)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	return &Handler{
		tokens:     &tokenSource{sa: sa, key: key, client: client},
		propertyID: cfg.GAPropertyID,
		gaLoc:      loc,
		siteURL:    cfg.GSCSiteURL,
		gaURL:      "https://analyticsdata.googleapis.com/v1beta/properties/",
		gscURL:     "https://searchconsole.googleapis.com/webmasters/v3/sites/",
		client:     client,
		log:        log,
	}, nil
}

type response struct {
	Status string `json:"status"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// GA serves GET /report?interval=hour|day&since=<YYYY-MM-DD>&until=<YYYY-MM-DD> (both inclusive).
func (h *Handler) GA() http.Handler {
	return h.route("/report", h.propertyID != "", func(ctx context.Context, interval string, since, until time.Time) (any, error) {
		return h.gaReport(ctx, interval, since, until)
	})
}

// GSC serves GET /query?interval=hour|day&since=<YYYY-MM-DD>&until=<YYYY-MM-DD> (both inclusive).
func (h *Handler) GSC() http.Handler {
	return h.route("/query", h.siteURL != "", func(ctx context.Context, interval string, since, until time.Time) (any, error) {
		return h.gscQuery(ctx, interval, since, until)
	})
}

func (h *Handler) route(path string, enabled bool, run func(context.Context, string, time.Time, time.Time) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path || !enabled {
			writeJSON(w, http.StatusNotFound, response{Status: "error", Error: "not found"})
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, response{Status: "error", Error: "method not allowed"})
			return
		}
		q := r.URL.Query()
		interval := q.Get("interval")
		since, err1 := time.Parse(time.DateOnly, q.Get("since"))
		until, err2 := time.Parse(time.DateOnly, q.Get("until"))
		if (interval != "hour" && interval != "day") || err1 != nil || err2 != nil || until.Before(since) {
			writeJSON(w, http.StatusBadRequest, response{Status: "error", Error: "need interval=hour|day and YYYY-MM-DD since <= until"})
			return
		}
		result, err := run(r.Context(), interval, since, until)
		if err != nil {
			h.log.Error("google report failed", "path", path, "err", err)
			writeJSON(w, http.StatusBadGateway, response{Status: "error", Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, response{Status: "ok", Result: result})
	})
}

func (h *Handler) gaReport(ctx context.Context, interval string, since, until time.Time) ([]GARow, error) {
	dims := []map[string]string{{"name": "date"}}
	layout := "20060102"
	if interval == "hour" {
		dims = append(dims, map[string]string{"name": "hour"})
		layout = "20060102 15"
	}
	var payload struct {
		Rows []struct {
			DimensionValues []struct{ Value string } `json:"dimensionValues"`
			MetricValues    []struct{ Value string } `json:"metricValues"`
		} `json:"rows"`
	}
	err := h.post(ctx, gaScope, h.gaURL+h.propertyID+":runReport", map[string]any{
		"dateRanges":    []map[string]string{{"startDate": since.Format(time.DateOnly), "endDate": until.Format(time.DateOnly)}},
		"dimensions":    dims,
		"metrics":       []map[string]string{{"name": "sessions"}, {"name": "screenPageViews"}, {"name": "totalUsers"}, {"name": "activeUsers"}},
		"keepEmptyRows": true,
	}, &payload)
	if err != nil {
		return nil, fmt.Errorf("ga api: %w", err)
	}
	rows := []GARow{}
	for _, r := range payload.Rows {
		if len(r.DimensionValues) < 1 || len(r.MetricValues) < 4 {
			continue
		}
		parts := make([]string, len(r.DimensionValues))
		for i, d := range r.DimensionValues {
			parts[i] = d.Value
		}
		t, err := time.ParseInLocation(layout, strings.Join(parts, " "), h.gaLoc)
		if err != nil {
			continue
		}
		timeslot := t.Format(time.DateOnly)
		if interval == "hour" {
			timeslot = t.UTC().Format(time.RFC3339)
		}
		rows = append(rows, GARow{
			Timeslot:        timeslot,
			Sessions:        atoi(r.MetricValues[0].Value),
			ScreenPageViews: atoi(r.MetricValues[1].Value),
			ActiveUsers:     atoi(r.MetricValues[3].Value),
		})
	}
	return rows, nil
}

func (h *Handler) gscQuery(ctx context.Context, interval string, since, until time.Time) ([]GSCRow, error) {
	body := map[string]any{"startDate": since.Format(time.DateOnly), "endDate": until.Format(time.DateOnly), "rowLimit": 25000}
	if interval == "hour" {
		body["dimensions"] = []string{"hour"}
		body["dataState"] = "hourly_all"
	} else {
		body["dimensions"] = []string{"date"}
	}
	var payload struct {
		Rows []struct {
			Keys        []string `json:"keys"`
			Clicks      float64  `json:"clicks"`
			Impressions float64  `json:"impressions"`
			CTR         float64  `json:"ctr"`
			Position    float64  `json:"position"`
		} `json:"rows"`
	}
	if err := h.post(ctx, gscScope, h.gscURL+url.PathEscape(h.siteURL)+"/searchAnalytics/query", body, &payload); err != nil {
		return nil, fmt.Errorf("gsc api: %w", err)
	}
	rows := []GSCRow{}
	for _, r := range payload.Rows {
		if len(r.Keys) < 1 {
			continue
		}
		var timeslot string
		if interval == "hour" {
			t, ok := parseGSCHour(r.Keys[0])
			if !ok {
				continue
			}
			timeslot = t.UTC().Format(time.RFC3339)
		} else {
			if len(r.Keys[0]) != 10 {
				continue
			}
			timeslot = r.Keys[0]
		}
		rows = append(rows, GSCRow{
			Timeslot:    timeslot,
			Clicks:      int(r.Clicks),
			Impressions: int(r.Impressions),
			CTR:         round4(r.CTR * 100),
			Position:    round4(r.Position),
		})
	}
	return rows, nil
}

// parseGSCHour reads Search Console's hour key (local Pacific time) and truncates it to the hour.
func parseGSCHour(raw string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15", "2006-01-02T15", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, raw, gscLoc); err == nil {
			return t.In(gscLoc).Truncate(time.Hour), true
		}
	}
	return time.Time{}, false
}

func (h *Handler) post(ctx context.Context, scope, endpoint string, body, out any) error {
	token, err := h.tokens.token(ctx, scope)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%d %s", resp.StatusCode, string(b))
	}
	return json.Unmarshal(b, out)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// round4 matches zengine's former rounding (half up at the 4th decimal).
func round4(v float64) float64 {
	return float64(int(v*10000+0.5)) / 10000
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
