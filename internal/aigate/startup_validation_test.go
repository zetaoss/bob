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
