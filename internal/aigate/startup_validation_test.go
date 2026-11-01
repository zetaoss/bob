package aigate

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"bob/internal/config"
)

func TestValidateStartupModels_Assume(t *testing.T) {
	var requestedModels []string
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)

		mu.Lock()
		requestedModels = append(requestedModels, req.Model)
		mu.Unlock()

		if req.Model == "model-fail" {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		resp := chatCompletionResponse{
			Choices: []chatCompletionChoice{
				{Message: chatCompletionMessage{Role: "assistant", Content: "hello"}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	oldEndpoint := geminiDefaultEndpoint
	geminiDefaultEndpoint = ts.URL
	t.Cleanup(func() { geminiDefaultEndpoint = oldEndpoint })

	cfg := &config.AIGateConfig{
		Models: []string{
			"gemini/model-assumed-avail",
			"gemini/model-assumed-unavail",
			"gemini/model-ok",
			"gemini/model-fail",
		},
		AssumeAvailable:   []string{"gemini/model-assumed-avail"},
		AssumeUnavailable: []string{"gemini/model-assumed-unavail"},
		Providers: map[string]config.ProviderConfig{
			"gemini": {APIKey: "test-key"},
		},
	}

	g, err := NewGateway(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("failed to create gateway: %v", err)
	}

	// Mock provider model list
	g.availableModels["gemini"] = []string{
		"model-ok",
		"model-fail",
	}

	if err := g.ValidateStartupModels(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	// 1. Probed requests should only be model-ok and model-fail.
	// Assumed models must not be probed via HTTP.
	for _, m := range requestedModels {
		if m == "model-assumed-avail" || m == "model-assumed-unavail" {
			t.Errorf("model %q should have skipped probe request, but was probed", m)
		}
	}
	if !slices.Contains(requestedModels, "model-ok") {
		t.Errorf("expected model-ok to be probed, got: %v", requestedModels)
	}
	if !slices.Contains(requestedModels, "model-fail") {
		t.Errorf("expected model-fail to be probed, got: %v", requestedModels)
	}

	// 2. Check enabled models (modelOrder)
	expectedOrder := []string{"gemini/model-assumed-avail", "gemini/model-ok"}
	if !slices.Equal(g.modelOrder, expectedOrder) {
		t.Errorf("expected modelOrder %v, got %v", expectedOrder, g.modelOrder)
	}

	// 3. Check modelRoutes
	if _, ok := g.modelRoutes["gemini/model-assumed-avail"]; !ok {
		t.Errorf("expected model-assumed-avail to remain in modelRoutes")
	}
	if _, ok := g.modelRoutes["gemini/model-assumed-unavail"]; ok {
		t.Errorf("expected model-assumed-unavail to be removed from modelRoutes")
	}
	if _, ok := g.modelRoutes["gemini/model-ok"]; !ok {
		t.Errorf("expected model-ok to remain in modelRoutes")
	}
	if _, ok := g.modelRoutes["gemini/model-fail"]; ok {
		t.Errorf("expected model-fail to be removed from modelRoutes")
	}
}

func TestValidateStartupModels_UntrimmedAssumption(t *testing.T) {
	cfg := &config.AIGateConfig{
		Models: []string{
			"gemini/model-a",
			"gemini/model-b",
		},
		AssumeAvailable:   []string{" gemini/model-a "},
		AssumeUnavailable: []string{" gemini/model-b "},
		Providers: map[string]config.ProviderConfig{
			"gemini": {APIKey: "test-key"},
		},
	}

	g, err := NewGateway(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("failed to create gateway: %v", err)
	}

	if err := g.ValidateStartupModels(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	if _, ok := g.modelRoutes["gemini/model-a"]; !ok {
		t.Errorf("expected trimmed gemini/model-a to be recognized as assumed-available")
	}
	if _, ok := g.modelRoutes["gemini/model-b"]; ok {
		t.Errorf("expected trimmed gemini/model-b to be recognized as assumed-unavailable and removed")
	}
}

func TestValidateStartupModels_OllamaProbeNormalization(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Native Ollama response has `message`, not `choices`
		resp := ollamaChatResponse{
			Model: "gemma:2b",
			Message: struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}{
				Role:    "assistant",
				Content: "hello from ollama",
			},
			DoneReason: "stop",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	oldEndpoint := ollamaDefaultEndpoint
	ollamaDefaultEndpoint = ts.URL
	t.Cleanup(func() { ollamaDefaultEndpoint = oldEndpoint })

	cfg := &config.AIGateConfig{
		Models: []string{"ollama/gemma:2b"},
		Providers: map[string]config.ProviderConfig{
			"ollama": {},
		},
	}

	g, err := NewGateway(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("failed to create gateway: %v", err)
	}
	g.availableModels["ollama"] = []string{"gemma:2b"}

	if err := g.ValidateStartupModels(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	if _, ok := g.modelRoutes["ollama/gemma:2b"]; !ok {
		t.Errorf("expected ollama model to remain active after normalized probe response")
	}
}
