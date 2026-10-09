// Package proxy forwards a route prefix to an upstream service.
package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	"bob/internal/logging"
)

// New returns a handler that forwards /<name>/<rest> to <target>/<rest>, keeping the query string.
// Responses are flushed immediately so streaming upstreams pass through.
func New(name, target string, log *logging.Logger) (http.Handler, error) {
	upstream, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: parse upstream: %w", name, err)
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.SetXForwarded()
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Errorf("proxy %s: %s %s: %v", name, r.Method, r.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "upstream unavailable", "route": name})
		},
	}
	return http.StripPrefix("/"+name, rp), nil
}
