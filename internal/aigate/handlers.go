package aigate

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"bob/internal/config"
)

// Gateway routes OpenAI-style chat completions to configured providers, with fallback across models on request.
// Moved from github.com/zetaoss/aigate; bob serves it under /aigate.
type Gateway struct {
	cfg                  *config.AIGateConfig
	log                  *slog.Logger
	providerMap          map[string]config.ProviderConfig
	modelRoutes          map[string]ModelRoute
	modelOrder           []string
	availableModels      map[string][]string
	availableModelErrors map[string]error
	usage                *usageTracker
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
	// Fallback true lets a request for a configured model fall back to the other models. It is not sent to providers.
	Fallback bool `json:"fallback,omitempty"`
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
		usage:                newUsageTracker(time.Now),
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

	modelChain, badModel := g.resolveModelChain(req.Model, req.Fallback)
	if badModel {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "model not configured", "model": req.Model})
		return
	}
	if len(modelChain) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no models configured"})
		return
	}

	responseBody, resolvedModel, attemptedModels, err := g.callWithFallback(req, modelChain)
	var cooling *allCoolingDownError
	if errors.As(err, &cooling) {
		g.log.Error("chat completion skipped: all models cooling down", "model", req.Model, "until", cooling.until)
		w.Header().Set("Retry-After", strconv.Itoa(max(int(math.Ceil(cooling.until.Sub(g.usage.now()).Seconds())), 1)))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error(), "retry_after": cooling.until})
		return
	}
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

// resolveModelChain returns the models to try: every configured model when none is requested, a provider's
// models for a provider name, and for a configured model that model alone, or followed by the rest with fallback.
func (g *Gateway) resolveModelChain(requestedModel string, fallback bool) ([]string, bool) {
	if requestedModel == "" {
		return g.interleaveByProvider(g.modelOrder), false
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
	if !fallback {
		return chain, false
	}
	for _, model := range g.modelOrder {
		if model == requestedModel {
			continue
		}
		chain = append(chain, model)
	}
	return g.interleaveByProvider(chain), false
}

// interleaveByProvider reorders a fallback chain so consecutive models come from different providers, keeping
// the first model first: providers take turns in order of first appearance, each in its configured order.
// One provider's outage (such as every Gemini model answering 503) then does not use up all the fallbacks.
func (g *Gateway) interleaveByProvider(chain []string) []string {
	var providers []string
	byProvider := map[string][]string{}
	for _, model := range chain {
		p := g.modelRoutes[model].Provider
		if _, seen := byProvider[p]; !seen {
			providers = append(providers, p)
		}
		byProvider[p] = append(byProvider[p], model)
	}
	out := make([]string, 0, len(chain))
	for len(out) < len(chain) {
		for _, p := range providers {
			if models := byProvider[p]; len(models) > 0 {
				out = append(out, models[0])
				byProvider[p] = models[1:]
			}
		}
	}
	return out
}

func (g *Gateway) callWithFallback(req ChatCompletionRequest, modelChain []string) ([]byte, string, []string, error) {
	if len(modelChain) == 0 {
		return nil, "", nil, fmt.Errorf("no models configured")
	}
	// One pass over the chain, calling at most the first available model and MaxFallbacks more.
	maxCalls := 1
	if g.cfg.Fallback.MaxFallbacks != nil {
		maxCalls += *g.cfg.Fallback.MaxFallbacks
	}
	attempted := make([]string, 0, maxCalls)
	var lastErr error
	var earliestRetry time.Time

	for _, modelID := range modelChain {
		if len(attempted) >= maxCalls {
			break
		}
		if until, cooling := g.usage.coolingDown(modelID); cooling {
			if earliestRetry.IsZero() || until.Before(earliestRetry) {
				earliestRetry = until
			}
			continue
		}
		attempted = append(attempted, modelID)
		route := g.modelRoutes[modelID]
		localReq := req
		localReq.Model = modelID

		body, header, err := g.callProvider(route, localReq)
		g.usage.record(modelID, header, err)
		if err == nil {
			normalizedBody, normalizeErr := normalizeProviderResponse(route.Provider, modelID, body)
			if normalizeErr != nil {
				return nil, "", attempted, fmt.Errorf("normalize response failed model=%s: %w", modelID, normalizeErr)
			}
			return normalizedBody, modelID, append([]string(nil), attempted...), nil
		}
		lastErr = err
		g.log.Error("model attempt failed", "model", modelID, "err", err)
	}

	if len(attempted) == 0 {
		return nil, "", nil, &allCoolingDownError{until: earliestRetry}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("request failed without provider error")
	}
	return nil, "", append([]string(nil), attempted...), fmt.Errorf("attempted models=%s: %w", strings.Join(attempted, ","), lastErr)
}

type allCoolingDownError struct {
	until time.Time
}

func (e *allCoolingDownError) Error() string {
	return fmt.Sprintf("all models are cooling down after quota errors until %s", e.until.Format(time.RFC3339))
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
	mux.HandleFunc("/v1/usage", g.UsageHandler)
	return mux
}
