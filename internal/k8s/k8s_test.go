package k8s

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bob/internal/config"
)

func vec(metric map[string]string, v string) []sample {
	return []sample{{Metric: metric, Value: []any{1712345678.0, v}}}
}

func fakePrometheus(t *testing.T, results map[string][]sample, gotTime *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotTime != nil {
			*gotTime = r.URL.Query().Get("time")
		}
		body := map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": results[r.URL.Query().Get("query")]}}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func handler(prom string) *Handler {
	return NewHandler(config.K8sConfig{Prometheus: prom, Nodepool: "pool-a", Namespace: "prod3", PVC: "db"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

var pvcUsed = `max by (namespace, persistentvolumeclaim) (kubelet_volume_stats_used_bytes{namespace="prod3", persistentvolumeclaim="db"})`
var pvcCapacity = `max by (namespace, persistentvolumeclaim) (kubelet_volume_stats_capacity_bytes{namespace="prod3", persistentvolumeclaim="db"})`

func TestCollect(t *testing.T) {
	node := map[string]string{"node": "my-cluster-pool-a-node-1"}
	pod := map[string]string{"pod": "web-app-1", "namespace": "prod3"}
	pvc := map[string]string{"persistentvolumeclaim": "db", "namespace": "prod3"}
	var gotTime string
	prom := fakePrometheus(t, map[string][]sample{
		`rate(node_cpu_usage_seconds_total{node=~".*pool-a.*"}[5m])`:                    vec(node, "0.833"),
		`kube_node_status_allocatable{node=~".*pool-a.*", resource="cpu", unit="core"}`: vec(node, "1.93"),
		`node_memory_working_set_bytes{node=~".*pool-a.*"}`:                             vec(node, "8217997312"),
		`kube_node_status_allocatable{node=~".*pool-a.*", resource="memory"}`:           vec(node, "13918449664"),
		`rate(pod_cpu_usage_seconds_total{namespace="prod3"}[5m])`:                      vec(pod, "0.45"),
		`pod_memory_working_set_bytes{namespace="prod3"}`:                               vec(pod, "524288000"),
		pvcUsed:     vec(pvc, "11811160064"),
		pvcCapacity: vec(pvc, "106300440576"),
		`increase(zeta_defender_fighting_seconds_total[1h]) / 3600`: vec(nil, "0.25"),
		`max(max_over_time(zeta_defender_level[1h]))`:               vec(nil, "7"),
	}, &gotTime)

	at := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	m, err := handler(prom).Collect(context.Background(), &at)
	if err != nil {
		t.Fatal(err)
	}
	if gotTime != "2026-08-15T12:00:00Z" {
		t.Errorf("evaluation time not sent: %q", gotTime)
	}
	if len(m.Nodes) != 1 || m.Nodes[0] != (Node{Name: "my-cluster-pool-a-node-1", CPUUsage: 0.833, CPUAllocatable: 1.93, MemoryUsage: 8217997312, MemoryAllocatable: 13918449664}) {
		t.Errorf("nodes=%+v", m.Nodes)
	}
	if len(m.Pods) != 1 || m.Pods[0] != (Pod{Name: "web-app-1", Namespace: "prod3", CPUUsage: 0.45, MemoryUsage: 524288000}) {
		t.Errorf("pods=%+v", m.Pods)
	}
	if len(m.PVCs) != 1 || m.PVCs[0].Usage != 11811160064 || m.PVCs[0].Capacity != 106300440576 || math.Abs(m.PVCs[0].UsagePercent-11.11111111111111) > 1e-12 {
		t.Errorf("pvcs=%+v", m.PVCs)
	}
	if m.Defender != (Defender{FightingRatio: 0.25, MaxLevel: 7}) || m.Nodepool != "pool-a" || m.Namespace != "prod3" {
		t.Errorf("metrics=%+v", m)
	}
}

func TestCollect_MissingCapacity(t *testing.T) {
	prom := fakePrometheus(t, map[string][]sample{
		pvcUsed: vec(map[string]string{"persistentvolumeclaim": "db", "namespace": "prod3"}, "0"),
	}, nil)
	_, err := handler(prom).Collect(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("expected missing capacity error, got %v", err)
	}
}

func TestServeHTTP(t *testing.T) {
	pvc := map[string]string{"persistentvolumeclaim": "db", "namespace": "prod3"}
	h := handler(fakePrometheus(t, map[string][]sample{pvcUsed: vec(pvc, "1"), pvcCapacity: vec(pvc, "4")}, nil))

	for target, want := range map[string]int{
		"/metrics":                           http.StatusOK,
		"/metrics?time=2026-08-15T12:00:00Z": http.StatusOK,
		"/metrics?time=yesterday":            http.StatusBadRequest,
		"/other":                             http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != want {
			t.Errorf("%s: code=%d, want %d", target, rec.Code, want)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	var body struct {
		Status string  `json:"status"`
		Result Metrics `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Status != "ok" || body.Result.PVCs[0].UsagePercent != 25 {
		t.Fatalf("body=%s err=%v", rec.Body.String(), err)
	}
}
