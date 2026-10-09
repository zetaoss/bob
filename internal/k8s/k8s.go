// Package k8s reports cluster resource usage from Prometheus (moved from zengine's stat task):
// nodes of one node pool, pods and one PVC of one namespace, and zeta-defender activity.
package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"bob/internal/config"
)

type Node struct {
	Name              string  `json:"name"`
	CPUUsage          float64 `json:"cpu_usage"`
	CPUAllocatable    float64 `json:"cpu_allocatable"`
	MemoryUsage       float64 `json:"memory_usage"`
	MemoryAllocatable float64 `json:"memory_allocatable"`
}

type Pod struct {
	Name        string  `json:"name"`
	Namespace   string  `json:"namespace"`
	CPUUsage    float64 `json:"cpu_usage"`
	MemoryUsage float64 `json:"memory_usage"`
}

type PVC struct {
	Name         string  `json:"name"`
	Namespace    string  `json:"namespace"`
	Usage        float64 `json:"usage"`
	Capacity     float64 `json:"capacity"`
	UsagePercent float64 `json:"usage_percent"`
}

type Defender struct {
	FightingRatio float64 `json:"fighting_ratio"`
	MaxLevel      float64 `json:"max_level"`
}

// Metrics is the /k8s/metrics result. CPU is in cores, memory and storage in bytes.
type Metrics struct {
	Nodepool  string   `json:"nodepool"`
	Namespace string   `json:"namespace"`
	Nodes     []Node   `json:"nodes"`
	Pods      []Pod    `json:"pods"`
	PVCs      []PVC    `json:"pvcs"`
	Defender  Defender `json:"defender"`
}

type Handler struct {
	cfg    config.K8sConfig
	client *http.Client
	log    *slog.Logger
}

func NewHandler(cfg config.K8sConfig, log *slog.Logger) *Handler {
	return &Handler{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, log: log}
}

type response struct {
	Status string `json:"status"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ServeHTTP answers GET /metrics[?time=<RFC3339>]; without time, the current values are returned.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		writeJSON(w, http.StatusNotFound, response{Status: "error", Error: "not found"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, response{Status: "error", Error: "method not allowed"})
		return
	}
	var at *time.Time
	if v := r.URL.Query().Get("time"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, response{Status: "error", Error: "time must be RFC3339"})
			return
		}
		at = &t
	}
	m, err := h.Collect(r.Context(), at)
	if err != nil {
		h.log.Error("k8s metrics failed", "err", err)
		writeJSON(w, http.StatusBadGateway, response{Status: "error", Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, response{Status: "ok", Result: m})
}

// Collect queries Prometheus. Node and pod queries are best effort (a failed query leaves its
// field at zero); a missing PVC usage or capacity is an error.
func (h *Handler) Collect(ctx context.Context, at *time.Time) (Metrics, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	nodes, pods := h.nodesAndPods(ctx, at)
	pvc, err := h.pvc(ctx, at)
	if err != nil {
		return Metrics{}, err
	}
	return Metrics{
		Nodepool:  h.cfg.Nodepool,
		Namespace: h.cfg.Namespace,
		Nodes:     nodes,
		Pods:      pods,
		PVCs:      []PVC{pvc},
		Defender:  h.defender(ctx, at),
	}, nil
}

func (h *Handler) nodesAndPods(ctx context.Context, at *time.Time) ([]Node, []Pod) {
	nodepool := strings.Trim(h.cfg.Nodepool, "-")
	ns := h.cfg.Namespace
	nodeMap := map[string]*Node{}
	podMap := map[string]*Pod{}

	setNode := func(query string, set func(*Node, float64)) {
		for _, r := range h.queryBestEffort(ctx, query, at) {
			name := nodeName(r.Metric)
			if !keepNode(name, h.cfg.Nodepool) {
				continue
			}
			if v, ok := value(r.Value); ok {
				n, found := nodeMap[name]
				if !found {
					n = &Node{Name: name}
					nodeMap[name] = n
				}
				set(n, v)
			}
		}
	}
	setPod := func(query string, set func(*Pod, float64)) {
		for _, r := range h.queryBestEffort(ctx, query, at) {
			name := podName(r.Metric)
			podNS := r.Metric["namespace"]
			if podNS == "" {
				podNS = ns
			}
			if !keepPod(name, podNS, ns) {
				continue
			}
			if v, ok := value(r.Value); ok {
				p, found := podMap[name]
				if !found {
					p = &Pod{Name: name, Namespace: podNS}
					podMap[name] = p
				}
				set(p, v)
			}
		}
	}

	setNode(fmt.Sprintf(`rate(node_cpu_usage_seconds_total{node=~".*%s.*"}[5m])`, nodepool), func(n *Node, v float64) { n.CPUUsage = v })
	setNode(fmt.Sprintf(`kube_node_status_allocatable{node=~".*%s.*", resource="cpu", unit="core"}`, nodepool), func(n *Node, v float64) { n.CPUAllocatable = v })
	setNode(fmt.Sprintf(`node_memory_working_set_bytes{node=~".*%s.*"}`, nodepool), func(n *Node, v float64) { n.MemoryUsage = v })
	setNode(fmt.Sprintf(`kube_node_status_allocatable{node=~".*%s.*", resource="memory"}`, nodepool), func(n *Node, v float64) { n.MemoryAllocatable = v })
	setPod(fmt.Sprintf(`rate(pod_cpu_usage_seconds_total{namespace=%q}[5m])`, ns), func(p *Pod, v float64) { p.CPUUsage = v })
	setPod(fmt.Sprintf(`pod_memory_working_set_bytes{namespace=%q}`, ns), func(p *Pod, v float64) { p.MemoryUsage = v })

	nodes := make([]Node, 0, len(nodeMap))
	for _, n := range nodeMap {
		nodes = append(nodes, *n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	pods := make([]Pod, 0, len(podMap))
	for _, p := range podMap {
		pods = append(pods, *p)
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return nodes, pods
}

func (h *Handler) pvc(ctx context.Context, at *time.Time) (PVC, error) {
	ns, name := h.cfg.Namespace, h.cfg.PVC
	one := func(metric string) (float64, bool, error) {
		res, err := h.query(ctx, fmt.Sprintf(`max by (namespace, persistentvolumeclaim) (%s{namespace=%q, persistentvolumeclaim=%q})`, metric, ns, name), at)
		if err != nil {
			return 0, false, err
		}
		for _, r := range res {
			if r.Metric["namespace"] == ns && r.Metric["persistentvolumeclaim"] == name {
				v, ok := value(r.Value)
				return v, ok, nil
			}
		}
		return 0, false, nil
	}
	usage, ok, err := one("kubelet_volume_stats_used_bytes")
	if err != nil {
		return PVC{}, fmt.Errorf("query PVC usage: %w", err)
	}
	if !ok {
		return PVC{}, fmt.Errorf("missing PVC usage metric for %s/%s", ns, name)
	}
	capacity, ok, err := one("kubelet_volume_stats_capacity_bytes")
	if err != nil {
		return PVC{}, fmt.Errorf("query PVC capacity: %w", err)
	}
	if !ok || capacity <= 0 {
		return PVC{}, fmt.Errorf("missing or invalid PVC capacity metric for %s/%s", ns, name)
	}
	return PVC{Name: name, Namespace: ns, Usage: usage, Capacity: capacity, UsagePercent: usage / capacity * 100}, nil
}

func (h *Handler) defender(ctx context.Context, at *time.Time) Defender {
	var d Defender
	if res := h.queryBestEffort(ctx, `increase(zeta_defender_fighting_seconds_total[1h]) / 3600`, at); len(res) > 0 {
		d.FightingRatio, _ = value(res[0].Value)
	}
	if res := h.queryBestEffort(ctx, `max(max_over_time(zeta_defender_level[1h]))`, at); len(res) > 0 {
		d.MaxLevel, _ = value(res[0].Value)
	}
	return d
}

type sample struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

func (h *Handler) queryBestEffort(ctx context.Context, q string, at *time.Time) []sample {
	res, err := h.query(ctx, q, at)
	if err != nil {
		h.log.Debug("prometheus query failed", "query", q, "err", err)
		return nil
	}
	return res
}

func (h *Handler) query(ctx context.Context, q string, at *time.Time) ([]sample, error) {
	params := url.Values{"query": []string{q}}
	if at != nil {
		params.Set("time", at.UTC().Format(time.RFC3339))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h.cfg.Prometheus, "/")+"/api/v1/query?"+params.Encode(), nil)
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
		Data   struct {
			Result []sample `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus status %s", body.Status)
	}
	return body.Data.Result, nil
}

func nodeName(m map[string]string) string {
	for _, k := range []string{"node", "name", "instance", "kubernetes_node"} {
		if m[k] != "" {
			return m[k]
		}
	}
	return ""
}

func podName(m map[string]string) string {
	for _, k := range []string{"pod", "name", "pod_name"} {
		if m[k] != "" {
			return m[k]
		}
	}
	return ""
}

func keepNode(name, nodepool string) bool {
	if name == "" || strings.EqualFold(name, "total") || strings.EqualFold(name, nodepool) {
		return false
	}
	if nodepool == "" {
		return true
	}
	return strings.Contains(name, "-"+strings.Trim(nodepool, "-")+"-") || strings.Contains(name, nodepool)
}

func keepPod(name, podNS, namespace string) bool {
	if name == "" || strings.EqualFold(name, "total") || strings.EqualFold(name, namespace) {
		return false
	}
	return namespace == "" || podNS == "" || podNS == namespace
}

func value(v []any) (float64, bool) {
	if len(v) < 2 {
		return 0, false
	}
	switch x := v[1].(type) {
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	case float64:
		return x, true
	}
	return 0, false
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
