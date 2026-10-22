package aigate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

func (g *Gateway) LoadAvailableModels(ctx context.Context) {
	providers := make([]string, 0, len(g.providerMap))
	for provider := range g.providerMap {
		providers = append(providers, provider)
	}
	slices.Sort(providers)

	for _, provider := range providers {
		models, err := g.fetchAvailableModels(ctx, provider)
		if err != nil {
			g.availableModelErrors[provider] = err
			g.log.Error("available model load failed", "provider", provider, "err", err)
			continue
		}
		g.availableModels[provider] = models
		delete(g.availableModelErrors, provider)
		g.log.Info("available model load passed", "provider", provider, "count", len(models), "models", models)
	}
}

func (g *Gateway) ValidateStartupModels() error {
	for publicModel, route := range g.modelRoutes {
		if err := g.validateModelAvailability(route); err != nil {
			return fmt.Errorf("startup model validation failed for %q: %w", publicModel, err)
		}
		g.log.Info("startup model validation passed", "model", publicModel, "provider", route.Provider)
	}
	return nil
}

func (g *Gateway) validateModelAvailability(route ModelRoute) error {
	if err, ok := g.availableModelErrors[route.Provider]; ok {
		return fmt.Errorf("provider model list unavailable: %w", err)
	}

	models := g.availableModels[route.Provider]
	for _, model := range models {
		if model == route.UpstreamModel {
			return nil
		}
	}

	baseErr := fmt.Errorf("model %q is not available from provider %q", route.UpstreamModel, route.Provider)
	if len(models) == 0 {
		return baseErr
	}
	return g.withAvailableModels(route.Provider, baseErr)
}

func (g *Gateway) withAvailableModels(provider string, baseErr error) error {
	models := g.availableModels[provider]
	if len(models) == 0 {
		return baseErr
	}
	maxShown := 20
	if len(models) > maxShown {
		models = models[:maxShown]
	}
	return fmt.Errorf("%w; available %s models: %s", baseErr, provider, strings.Join(models, ", "))
}

func (g *Gateway) fetchAvailableModels(ctx context.Context, provider string) ([]string, error) {
	switch strings.ToLower(provider) {
	case "ollama":
		return g.fetchOllamaModels(ctx)
	case "gemini":
		return g.fetchGeminiModels(ctx)
	case "groq":
		return g.fetchGroqModels(ctx)
	case "cloudflare":
		return g.fetchCloudflareModels(ctx)
	default:
		return nil, fmt.Errorf("unsupported provider")
	}
}

func (g *Gateway) fetchOllamaModels(ctx context.Context) ([]string, error) {
	provider := g.providerMap["ollama"]
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ollama.com/api/tags", nil)
	if err != nil {
		return nil, err
	}
	if provider.APIKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", provider.APIKey))
	}
	respBytes, status, err := doHTTP(req, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("status=%d", status)
	}

	type modelEntry struct {
		Name string `json:"name"`
	}
	var payload struct {
		Models []modelEntry `json:"models"`
	}
	if err := json.Unmarshal(respBytes, &payload); err != nil {
		return nil, err
	}

	models := make([]string, 0, len(payload.Models))
	for _, model := range payload.Models {
		if strings.TrimSpace(model.Name) != "" {
			models = append(models, model.Name)
		}
	}
	slices.Sort(models)
	return models, nil
}

func (g *Gateway) fetchGeminiModels(ctx context.Context) ([]string, error) {
	provider := g.providerMap["gemini"]
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models"
	if provider.APIKey != "" {
		endpoint = endpoint + "?key=" + url.QueryEscape(provider.APIKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	respBytes, status, err := doHTTP(req, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("status=%d", status)
	}

	type modelEntry struct {
		Name string `json:"name"`
	}
	var payload struct {
		Models []modelEntry `json:"models"`
	}
	if err := json.Unmarshal(respBytes, &payload); err != nil {
		return nil, err
	}

	models := make([]string, 0, len(payload.Models))
	for _, model := range payload.Models {
		name := strings.TrimSpace(model.Name)
		name = strings.TrimPrefix(name, "models/")
		if name != "" {
			models = append(models, name)
		}
	}
	slices.Sort(models)
	return models, nil
}

var groqModelsEndpoint = "https://api.groq.com/openai/v1/models"

func (g *Gateway) fetchGroqModels(ctx context.Context) ([]string, error) {
	provider := g.providerMap["groq"]
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, groqModelsEndpoint, nil)
	if err != nil {
		return nil, err
	}
	if provider.APIKey != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", provider.APIKey))
	}
	respBytes, status, err := doHTTP(req, 20*time.Second)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("status=%d", status)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &payload); err != nil {
		return nil, err
	}

	models := make([]string, 0, len(payload.Data))
	for _, model := range payload.Data {
		if id := strings.TrimSpace(model.ID); id != "" {
			models = append(models, id)
		}
	}
	slices.Sort(models)
	return models, nil
}

// fetchCloudflareModels lists Workers AI text generation models (names such as @cf/openai/gpt-oss-120b).
func (g *Gateway) fetchCloudflareModels(ctx context.Context) ([]string, error) {
	provider := g.providerMap["cloudflare"]
	const perPage = 100
	var models []string
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/accounts/%s/ai/models/search?task=%s&per_page=%d&page=%d",
			cloudflareAPIBase, url.PathEscape(provider.AccountID), url.QueryEscape("Text Generation"), perPage, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", provider.APIKey))
		respBytes, status, err := doHTTP(req, 20*time.Second)
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return nil, fmt.Errorf("status=%d", status)
		}
		var payload struct {
			Result []struct {
				Name string `json:"name"`
			} `json:"result"`
		}
		if err := json.Unmarshal(respBytes, &payload); err != nil {
			return nil, err
		}
		for _, m := range payload.Result {
			if name := strings.TrimSpace(m.Name); name != "" {
				models = append(models, name)
			}
		}
		if len(payload.Result) < perPage {
			break
		}
	}
	slices.Sort(models)
	return models, nil
}

func doHTTP(req *http.Request, timeout time.Duration) ([]byte, int, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response failed: %w", err)
	}
	return respBytes, resp.StatusCode, nil
}
