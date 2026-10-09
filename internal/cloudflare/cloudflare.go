// Package cloudflare reads zone analytics from the Cloudflare GraphQL API (moved from zengine's
// stat task) and returns them per timeslot as text values, the form zengine stores.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"bob/internal/config"
)

const graphqlURL = "https://api.cloudflare.com/client/v4/graphql"

// MetricNames are the analytics values returned for each timeslot.
var MetricNames = []string{
	"uniq_uniques",
	"sum_requests", "sum_pageViews", "sum_bytes", "sum_cachedBytes", "sum_cachedRequests",
	"sum_encryptedBytes", "sum_encryptedRequests", "sum_threats",
	"sum_browserMap", "sum_contentTypeMap", "sum_clientSSLMap", "sum_countryMap",
	"sum_ipClassMap", "sum_responseStatusMap", "sum_threatPathingMap",
}

// Group is one timeslot: a date (YYYY-MM-DD) for day, an RFC3339 datetime for hour.
type Group struct {
	Timeslot string            `json:"timeslot"`
	Metrics  map[string]string `json:"metrics"`
}

type Handler struct {
	token      string
	zoneID     string
	graphqlURL string
	client     *http.Client
	log        *slog.Logger
}

func NewHandler(cfg config.CloudflareConfig, log *slog.Logger) *Handler {
	return &Handler{token: cfg.APIToken, zoneID: cfg.ZoneID, graphqlURL: graphqlURL, client: &http.Client{Timeout: 20 * time.Second}, log: log}
}

type response struct {
	Status string  `json:"status"`
	Result []Group `json:"result,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// ServeHTTP answers GET /analytics?interval=hour&since=<RFC3339>&until=<RFC3339> and
// GET /analytics?interval=day&since=<YYYY-MM-DD>&until=<YYYY-MM-DD> (until exclusive).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/analytics" {
		writeJSON(w, http.StatusNotFound, response{Status: "error", Error: "not found"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, response{Status: "error", Error: "method not allowed"})
		return
	}
	q := r.URL.Query()
	var groups []Group
	var err error
	switch q.Get("interval") {
	case "hour":
		since, err1 := time.Parse(time.RFC3339, q.Get("since"))
		until, err2 := time.Parse(time.RFC3339, q.Get("until"))
		if err1 != nil || err2 != nil || !since.Before(until) {
			writeJSON(w, http.StatusBadRequest, response{Status: "error", Error: "hour needs RFC3339 since < until"})
			return
		}
		groups, err = h.Hourly(r.Context(), since, until)
	case "day":
		since, err1 := time.Parse(time.DateOnly, q.Get("since"))
		until, err2 := time.Parse(time.DateOnly, q.Get("until"))
		if err1 != nil || err2 != nil || !since.Before(until) {
			writeJSON(w, http.StatusBadRequest, response{Status: "error", Error: "day needs YYYY-MM-DD since < until"})
			return
		}
		groups, err = h.Daily(r.Context(), since, until)
	default:
		writeJSON(w, http.StatusBadRequest, response{Status: "error", Error: "interval must be hour or day"})
		return
	}
	if err != nil {
		h.log.Error("cloudflare analytics failed", "err", err)
		writeJSON(w, http.StatusBadGateway, response{Status: "error", Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, response{Status: "ok", Result: groups})
}

// Hourly queries hourly groups in windows of at most 24 hours.
func (h *Handler) Hourly(ctx context.Context, since, until time.Time) ([]Group, error) {
	var out []Group
	for start := since; start.Before(until); start = start.Add(24 * time.Hour) {
		end := start.Add(24 * time.Hour)
		if end.After(until) {
			end = until
		}
		groups, err := h.query(ctx, hourlyQuery, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
		if err != nil {
			return nil, err
		}
		out = append(out, groups...)
	}
	return out, nil
}

// Daily queries daily groups for dates in [since, until).
func (h *Handler) Daily(ctx context.Context, since, until time.Time) ([]Group, error) {
	return h.query(ctx, dailyQuery, since.Format(time.DateOnly), until.Format(time.DateOnly))
}

func (h *Handler) query(ctx context.Context, query, since, until string) ([]Group, error) {
	body, _ := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]any{"zoneTag": h.zoneID, "since": since, "until": until},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.graphqlURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("cloudflare api failed: %d %s", resp.StatusCode, string(raw))
	}
	var payload struct {
		Errors []any `json:"errors"`
		Data   struct {
			Viewer struct {
				Zones []struct {
					Zones []map[string]any `json:"zones"`
				} `json:"zones"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if len(payload.Errors) > 0 {
		return nil, fmt.Errorf("cloudflare graphql returned errors: %+v", payload.Errors)
	}
	if len(payload.Data.Viewer.Zones) == 0 {
		return []Group{}, nil
	}
	groups := []Group{}
	for _, g := range payload.Data.Viewer.Zones[0].Zones {
		dims, _ := g["dimensions"].(map[string]any)
		timeslot, _ := dims["timeslot"].(string)
		if timeslot == "" {
			continue
		}
		groups = append(groups, Group{Timeslot: timeslot, Metrics: metrics(g)})
	}
	return groups, nil
}

// metrics flattens one group the way zengine stored it: numbers as fmt %v of the decoded JSON
// value, maps as JSON text; a section missing from the response leaves its names out.
func metrics(group map[string]any) map[string]string {
	out := map[string]string{}
	if uniq, ok := group["uniq"].(map[string]any); ok {
		out["uniq_uniques"] = text(uniq["uniques"])
	}
	if sum, ok := group["sum"].(map[string]any); ok {
		for _, name := range MetricNames[1:] {
			out[name] = text(sum[name[len("sum_"):]])
		}
	}
	return out
}

func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case map[string]any, []any:
		b, err := json.Marshal(x)
		if err != nil {
			return "[]"
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

const dailyQuery = `query GetZoneAnalytics($zoneTag: string, $since: string, $until: string) {
  viewer {
    zones(filter: { zoneTag: $zoneTag }) {
      zones: httpRequests1dGroups(orderBy: [date_ASC], limit: 10000, filter: { date_geq: $since, date_lt: $until }) {
        dimensions { timeslot: date }
        uniq { uniques }
        sum {
          browserMap { pageViews key: uaBrowserFamily }
          bytes cachedBytes cachedRequests encryptedBytes encryptedRequests pageViews requests threats
          contentTypeMap { bytes requests key: edgeResponseContentTypeName }
          clientSSLMap { requests key: clientSSLProtocol }
          countryMap { bytes requests threats key: clientCountryName }
          ipClassMap { requests key: ipType }
          responseStatusMap { requests key: edgeResponseStatus }
          threatPathingMap { requests key: threatPathingName }
        }
      }
    }
  }
}`

const hourlyQuery = `query GetZoneAnalytics($zoneTag: string, $since: string, $until: string) {
  viewer {
    zones(filter: { zoneTag: $zoneTag }) {
      zones: httpRequests1hGroups(orderBy: [datetime_ASC], limit: 10000, filter: { datetime_geq: $since, datetime_lt: $until }) {
        dimensions { timeslot: datetime }
        uniq { uniques }
        sum {
          browserMap { pageViews key: uaBrowserFamily }
          bytes cachedBytes cachedRequests encryptedBytes encryptedRequests pageViews requests threats
          contentTypeMap { bytes requests key: edgeResponseContentTypeName }
          clientSSLMap { requests key: clientSSLProtocol }
          countryMap { bytes requests threats key: clientCountryName }
          ipClassMap { requests key: ipType }
          responseStatusMap { requests key: edgeResponseStatus }
          threatPathingMap { requests key: threatPathingName }
        }
      }
    }
  }
}`
