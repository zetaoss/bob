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

// GARow is one GA4 timeslot: RFC3339 UTC for hour, the property's local date (YYYY-MM-DD) for day.
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
	client := &http.Client{Timeout: 20 * time.Second}
	return &Handler{
		tokens:     &tokenSource{sa: sa, key: key, client: client},
		propertyID: cfg.GAPropertyID,
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

// GA serves GET /report?interval=hour|day&since=<RFC3339>&until=<RFC3339>: hours starting in
// [since, until), or the property-local dates overlapping it. Dates (YYYY-MM-DD, both inclusive,
// property-local) are also accepted. The property's time zone comes from the GA response.
func (h *Handler) GA() http.Handler {
	return h.route("/report", h.propertyID != "", func(ctx context.Context, interval, since, until string) (any, int, error) {
		if s, err1 := time.Parse(time.RFC3339, since); err1 == nil {
			u, err2 := time.Parse(time.RFC3339, until)
			if err2 != nil || !s.Before(u) {
				return nil, http.StatusBadRequest, fmt.Errorf("need RFC3339 since < until")
			}
			rows, err := h.gaReport(ctx, interval, s, u, true)
			return rows, http.StatusBadGateway, err
		}
		s, err1 := time.Parse(time.DateOnly, since)
		u, err2 := time.Parse(time.DateOnly, until)
		if err1 != nil || err2 != nil || u.Before(s) {
			return nil, http.StatusBadRequest, fmt.Errorf("need RFC3339 since < until or YYYY-MM-DD since <= until")
		}
		rows, err := h.gaReport(ctx, interval, s, u, false)
		return rows, http.StatusBadGateway, err
	})
}

// GSC serves GET /query?interval=hour|day&since=<YYYY-MM-DD>&until=<YYYY-MM-DD> (both inclusive).
func (h *Handler) GSC() http.Handler {
	return h.route("/query", h.siteURL != "", func(ctx context.Context, interval, since, until string) (any, int, error) {
		s, err1 := time.Parse(time.DateOnly, since)
		u, err2 := time.Parse(time.DateOnly, until)
		if err1 != nil || err2 != nil || u.Before(s) {
			return nil, http.StatusBadRequest, fmt.Errorf("need YYYY-MM-DD since <= until")
		}
		rows, err := h.gscQuery(ctx, interval, s, u)
		return rows, http.StatusBadGateway, err
	})
}

func (h *Handler) route(path string, enabled bool, run func(ctx context.Context, interval, since, until string) (any, int, error)) http.Handler {
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
		if interval != "hour" && interval != "day" {
			writeJSON(w, http.StatusBadRequest, response{Status: "error", Error: "interval must be hour or day"})
			return
		}
		result, status, err := run(r.Context(), interval, q.Get("since"), q.Get("until"))
		if err != nil {
			if status != http.StatusBadRequest {
				h.log.Error("google report failed", "path", path, "err", err)
			}
			writeJSON(w, status, response{Status: "error", Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, response{Status: "ok", Result: result})
	})
}

// gaReport queries GA4. With instants, it asks for the surrounding UTC dates (a day of margin
// covers any time zone) and keeps the hours starting in [since, until), or the local dates
// overlapping it; with dates, it returns every row for those property-local dates.
func (h *Handler) gaReport(ctx context.Context, interval string, since, until time.Time, instants bool) ([]GARow, error) {
	startDate, endDate := since, until
	if instants {
		startDate, endDate = since.UTC().AddDate(0, 0, -1), until.UTC().AddDate(0, 0, 1)
	}
	dims := []map[string]string{{"name": "date"}}
	layout := "20060102"
	if interval == "hour" {
		dims = append(dims, map[string]string{"name": "hour"})
		layout = "20060102 15"
	}
	var payload struct {
		Metadata struct {
			TimeZone string `json:"timeZone"`
		} `json:"metadata"`
		Rows []struct {
			DimensionValues []struct{ Value string } `json:"dimensionValues"`
			MetricValues    []struct{ Value string } `json:"metricValues"`
		} `json:"rows"`
	}
	err := h.post(ctx, gaScope, h.gaURL+h.propertyID+":runReport", map[string]any{
		"dateRanges":    []map[string]string{{"startDate": startDate.Format(time.DateOnly), "endDate": endDate.Format(time.DateOnly)}},
		"dimensions":    dims,
		"metrics":       []map[string]string{{"name": "sessions"}, {"name": "screenPageViews"}, {"name": "totalUsers"}, {"name": "activeUsers"}},
		"keepEmptyRows": true,
	}, &payload)
	if err != nil {
		return nil, fmt.Errorf("ga api: %w", err)
	}
	loc := time.UTC
	if payload.Metadata.TimeZone != "" {
		if l, err := time.LoadLocation(payload.Metadata.TimeZone); err == nil {
			loc = l
		} else {
			h.log.Error("unknown GA property time zone; using UTC", "timeZone", payload.Metadata.TimeZone)
		}
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
		t, err := time.ParseInLocation(layout, strings.Join(parts, " "), loc)
		if err != nil {
			continue
		}
		if instants {
			end := t.Add(time.Hour)
			if interval == "day" {
				end = t.AddDate(0, 0, 1)
			}
			if interval == "hour" && (t.Before(since) || !t.Before(until)) || interval == "day" && (!t.Before(until) || !end.After(since)) {
				continue
			}
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
