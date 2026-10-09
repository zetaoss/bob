package aigate

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"bob/internal/config"
)

// Gateway routes OpenAI-style chat completions to configured providers with fallback.
// Moved from github.com/zetaoss/aigate; bob serves it under /aigate.
type Gateway struct {
	cfg                  *config.AIGateConfig
	log                  *slog.Logger
	providerMap          map[string]config.ProviderConfig
	modelRoutes          map[string]ModelRoute
	modelOrder           []string
	availableModels      map[string][]string
	availableModelErrors map[string]error
}

type ModelRoute struct {
	Provider      string
	UpstreamModel string
}

type ModelInfo struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
}

type ChatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func NewGateway(cfg *config.AIGateConfig, log *slog.Logger) (*Gateway, error) {
	modelRoutes := make(map[string]ModelRoute)
	modelOrder := make([]string, 0, len(cfg.Models))
	providerMap := make(map[string]config.ProviderConfig, len(cfg.Providers))
	for providerName, provider := range cfg.Providers {
		providerName = strings.TrimSpace(providerName)
		if providerName == "" {
			return nil, fmt.Errorf("provider requires a name")
		}
		providerMap[providerName] = provider
	}

	for _, model := range cfg.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		providerName, upstreamModel, ok := strings.Cut(model, "/")
		if !ok || providerName == "" || upstreamModel == "" {
			return nil, fmt.Errorf("model %q must use provider/model format", model)
		}
		if _, ok := providerMap[providerName]; !ok {
			return nil, fmt.Errorf("model %q references undefined provider %q", model, providerName)
		}
		if _, exists := modelRoutes[model]; exists {
			return nil, fmt.Errorf("duplicate model %q", model)
		}
		modelRoutes[model] = ModelRoute{Provider: providerName, UpstreamModel: upstreamModel}
		modelOrder = append(modelOrder, model)
	}

	if len(modelRoutes) == 0 {
		return nil, fmt.Errorf("no models configured")
	}

	return &Gateway{
		cfg:                  cfg,
		log:                  log,
		providerMap:          providerMap,
		modelRoutes:          modelRoutes,
		modelOrder:           modelOrder,
		availableModels:      make(map[string][]string, len(providerMap)),
		availableModelErrors: make(map[string]error),
	}, nil
}

func (g *Gateway) startupRoutes() []ModelRoute {
	routes := make([]ModelRoute, 0, len(g.modelRoutes))
	for _, route := range g.modelRoutes {
		routes = append(routes, route)
	}
	return routes
}

func (g *Gateway) HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (g *Gateway) ModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if wantsAllModels(r) {
		entries, errs := g.availableModelEntries("")
		payload := map[string]any{"mode": "available", "data": entries}
		if len(errs) > 0 {
			payload["errors"] = errs
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": "enabled", "data": g.enabledModelEntries("")})
}

func (g *Gateway) ModelsByProviderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	provider := strings.TrimPrefix(r.URL.Path, "/models/")
	if provider == r.URL.Path {
		provider = strings.TrimPrefix(r.URL.Path, "/v1/models/")
	}
	provider = strings.TrimSpace(provider)
	if provider == "" || strings.Contains(provider, "/") {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "provider not found"})
		return
	}
	if _, ok := g.providerMap[provider]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "provider not found", "provider": provider})
		return
	}
	if wantsAllModels(r) {
		entries, errs := g.availableModelEntries(provider)
		payload := map[string]any{"mode": "available", "provider": provider, "data": entries}
		if len(errs) > 0 {
			payload["errors"] = errs
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": "enabled", "provider": provider, "data": g.enabledModelEntries(provider)})
}

func (g *Gateway) enabledModelEntries(providerFilter string) []ModelInfo {
	entries := make([]ModelInfo, 0, len(g.modelRoutes))
	for model, route := range g.modelRoutes {
		if providerFilter != "" && route.Provider != providerFilter {
			continue
		}
		entries = append(entries, ModelInfo{ID: model, Provider: route.Provider, Enabled: true})
	}
	slices.SortFunc(entries, func(a, b ModelInfo) int {
		return strings.Compare(a.ID, b.ID)
	})

	return entries
}

func (g *Gateway) availableModelEntries(providerFilter string) ([]ModelInfo, []string) {
	enabledLookup := make(map[string]struct{}, len(g.modelRoutes))
	for model := range g.modelRoutes {
		enabledLookup[model] = struct{}{}
	}

	var providers []string
	if providerFilter != "" {
		providers = []string{providerFilter}
	} else {
		providers = make([]string, 0, len(g.providerMap))
		for provider := range g.providerMap {
			providers = append(providers, provider)
		}
		slices.Sort(providers)
	}

	entries := make([]ModelInfo, 0)
	errors := make([]string, 0)
	for _, provider := range providers {
		if err, ok := g.availableModelErrors[provider]; ok {
			errors = append(errors, fmt.Sprintf("provider=%s error=%v", provider, err))
			continue
		}
		models := g.availableModels[provider]
		for _, upstreamModel := range models {
			publicModel := provider + "/" + upstreamModel
			_, enabled := enabledLookup[publicModel]
			entries = append(entries, ModelInfo{
				ID:       publicModel,
				Provider: provider,
				Enabled:  enabled,
			})
		}
	}

	slices.SortFunc(entries, func(a, b ModelInfo) int {
		if byProvider := strings.Compare(a.Provider, b.Provider); byProvider != 0 {
			return byProvider
		}
		return strings.Compare(a.ID, b.ID)
	})
	return entries, errors
}

func wantsAllModels(r *http.Request) bool {
	query := r.URL.Query()
	if _, ok := query["all"]; ok {
		return true
	}
	return false
}

func (g *Gateway) ChatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body", "detail": err.Error()})
		return
	}

	modelChain, badModel := g.resolveModelChain(req.Model)
	if badModel {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "model not configured", "model": req.Model})
		return
	}
	if len(modelChain) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no models configured"})
		return
	}

	responseBody, resolvedModel, attemptedModels, err := g.callWithFallback(req, modelChain)
	if err != nil {
		g.log.Error("chat completion failed", "model", req.Model, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	g.log.Info("chat completion routed", "requested_model", req.Model, "resolved_model", resolvedModel)
	responseBody, err = addProviderMeta(responseBody, req.Model, attemptedModels)
	if err != nil {
		g.log.Error("chat completion metadata decoration failed", "model", req.Model, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBody)
}

func (g *Gateway) resolveModelChain(requestedModel string) ([]string, bool) {
	if requestedModel == "" {
		return append([]string(nil), g.modelOrder...), false
	}
	if _, isProvider := g.providerMap[requestedModel]; isProvider {
		providerChain := make([]string, 0)
		for _, model := range g.modelOrder {
			route := g.modelRoutes[model]
			if route.Provider == requestedModel {
				providerChain = append(providerChain, model)
			}
		}
		if len(providerChain) == 0 {
			return nil, true
		}
		return providerChain, false
	}
	if _, ok := g.modelRoutes[requestedModel]; !ok {
		return nil, true
	}
	chain := []string{requestedModel}
	for _, model := range g.modelOrder {
		if model == requestedModel {
			continue
		}
		chain = append(chain, model)
	}
	return chain, false
}

func (g *Gateway) callWithFallback(req ChatCompletionRequest, modelChain []string) ([]byte, string, []string, error) {
	if len(modelChain) == 0 {
		return nil, "", nil, fmt.Errorf("no models configured")
	}
	fallbackRounds := g.cfg.Fallback.Rounds
	if fallbackRounds < 1 {
		fallbackRounds = 2
	}
	attemptLimit := len(modelChain) * fallbackRounds
	attempted := make([]string, 0, attemptLimit)
	var lastErr error

	for i := 0; i < attemptLimit; i++ {
		modelID := modelChain[i%len(modelChain)]
		attempted = append(attempted, modelID)
		route := g.modelRoutes[modelID]
		localReq := req
		localReq.Model = modelID

		body, err := g.callProvider(route, localReq)
		if err == nil {
			normalizedBody, normalizeErr := normalizeProviderResponse(route.Provider, modelID, body)
			if normalizeErr != nil {
				return nil, "", attempted, fmt.Errorf("normalize response failed model=%s: %w", modelID, normalizeErr)
			}
			return normalizedBody, modelID, append([]string(nil), attempted...), nil
		}
		lastErr = err
		if i == attemptLimit-1 {
			return nil, "", append([]string(nil), attempted...), fmt.Errorf("attempted models=%s: %w", strings.Join(attempted, ","), err)
		}
		g.log.Error("fallback to next model after failure", "model", modelID, "err", err)
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("request failed without provider error")
	}
	return nil, "", append([]string(nil), attempted...), fmt.Errorf("attempted models=%s: %w", strings.Join(attempted, ","), lastErr)
}

func addProviderMeta(responseBody []byte, requestedModel string, attemptedModels []string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return nil, fmt.Errorf("parse normalized response: %w", err)
	}
	requested := any(nil)
	if strings.TrimSpace(requestedModel) != "" {
		requested = requestedModel
	}
	payload["provider_meta"] = map[string]any{
		"requested_model":  requested,
		"attempted_models": attemptedModels,
	}
	updated, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal response with provider_meta: %w", err)
	}
	return updated, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// Handler serves the aigate API. bob mounts it under /aigate, so paths here are relative to that prefix.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", g.HealthHandler)
	mux.HandleFunc("/v1/models", g.ModelsHandler)
	mux.HandleFunc("/v1/models/", g.ModelsByProviderHandler)
	mux.HandleFunc("/v1/chat/completions", g.ChatCompletionsHandler)
	return mux
}
