package google

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bob/internal/config"
)

type fake struct {
	tokens   int
	lastBody map[string]any
	lastPath string
}

func newTestHandler(t *testing.T, gaBody, gscBody string) (*Handler, *fake) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	f := &fake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			f.tokens++
			if r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || strings.Count(r.FormValue("assertion"), ".") != 2 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"access_token":"at","expires_in":3600}`)
		default:
			if r.Header.Get("Authorization") != "Bearer at" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f.lastPath = r.URL.EscapedPath()
			_ = json.NewDecoder(r.Body).Decode(&f.lastBody)
			if strings.Contains(r.URL.Path, ":runReport") {
				_, _ = io.WriteString(w, gaBody)
			} else {
				_, _ = io.WriteString(w, gscBody)
			}
		}
	}))
	t.Cleanup(srv.Close)
	sa, _ := json.Marshal(map[string]string{"client_email": "bot@example.iam", "private_key": pemKey, "token_uri": srv.URL + "/token"})
	h, err := NewHandler(config.GoogleConfig{ServiceAccount: string(sa), GAPropertyID: "123", GSCSiteURL: "sc-domain:example.com"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h.gaURL = srv.URL + "/ga/"
	h.gscURL = srv.URL + "/gsc/"
	return h, f
}

func get(t *testing.T, h http.Handler, target string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestGA(t *testing.T) {
	ga := `{"rows":[{"dimensionValues":[{"value":"20261009"},{"value":"07"}],"metricValues":[{"value":"5"},{"value":"9"},{"value":"4"},{"value":"3"}]}]}`
	h, f := newTestHandler(t, ga, "")

	code, body := get(t, h.GA(), "/report?interval=hour&since=2026-10-08&until=2026-10-09")
	if code != http.StatusOK || body != `{"status":"ok","result":[{"timeslot":"2026-10-09T07:00:00Z","sessions":5,"screen_page_views":9,"active_users":3}]}` {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if f.lastPath != "/ga/123:runReport" || len(f.lastBody["dimensions"].([]any)) != 2 {
		t.Errorf("path=%s body=%v", f.lastPath, f.lastBody)
	}

	daily := `{"rows":[{"dimensionValues":[{"value":"20261008"}],"metricValues":[{"value":"1"},{"value":"2"},{"value":"3"},{"value":"4"}]}]}`
	h2, _ := newTestHandler(t, daily, "")
	if code, body := get(t, h2.GA(), "/report?interval=day&since=2026-10-01&until=2026-10-08"); code != http.StatusOK || !strings.Contains(body, `"timeslot":"2026-10-08","sessions":1,"screen_page_views":2,"active_users":4`) {
		t.Errorf("daily: code=%d body=%s", code, body)
	}
	_, _ = get(t, h.GA(), "/report?interval=day&since=2026-10-01&until=2026-10-08")
	if f.tokens != 1 {
		t.Errorf("access token should be reused, requested %d times", f.tokens)
	}
}

func TestGSC(t *testing.T) {
	gsc := `{"rows":[{"keys":["2026-10-09T00:00:00-07:00"],"clicks":12,"impressions":340,"ctr":0.0352941176,"position":7.123456}]}`
	h, f := newTestHandler(t, "", gsc)

	code, body := get(t, h.GSC(), "/query?interval=hour&since=2026-10-08&until=2026-10-09")
	if code != http.StatusOK || body != `{"status":"ok","result":[{"timeslot":"2026-10-09T07:00:00Z","clicks":12,"impressions":340,"ctr":3.5294,"position":7.1235}]}` {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if f.lastPath != "/gsc/sc-domain:example.com/searchAnalytics/query" || f.lastBody["dataState"] != "hourly_all" {
		t.Errorf("path=%s body=%v", f.lastPath, f.lastBody)
	}
}

func TestBadRequests(t *testing.T) {
	h, _ := newTestHandler(t, "", "")
	for target, want := range map[string]int{
		"/report?interval=week&since=2026-10-01&until=2026-10-02": http.StatusBadRequest,
		"/report?interval=day&since=2026-10-05&until=2026-10-01":  http.StatusBadRequest,
		"/other": http.StatusNotFound,
	} {
		if code, _ := get(t, h.GA(), target); code != want {
			t.Errorf("%s: code=%d, want %d", target, code, want)
		}
	}
}
