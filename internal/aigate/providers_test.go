package aigate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildProviderBodyMaxTokens(t *testing.T) {
	req := ChatCompletionRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "hello"}},
	}

	// When MaxTokens is nil, Cloudflare gets defaultCloudflareMaxTokens (2048)
	cfBody := buildProviderBody("cloudflare", req)
	cfReq, ok := cfBody.(openAIRequestBody)
	if !ok || cfReq.MaxTokens == nil || *cfReq.MaxTokens != defaultCloudflareMaxTokens {
		t.Fatalf("expected cloudflare default max_tokens %d, got %+v", defaultCloudflareMaxTokens, cfReq.MaxTokens)
	}
	cfJSON, err := json.Marshal(cfBody)
	if err != nil {
		t.Fatalf("marshal cfBody: %v", err)
	}
	if want := `"max_tokens":2048`; !strings.Contains(string(cfJSON), want) {
		t.Fatalf("expected %q in cloudflare body json, got %s", want, string(cfJSON))
	}

	// Other providers leave MaxTokens as nil when omitted
	groqBody := buildProviderBody("groq", req)
	groqReq, ok := groqBody.(openAIRequestBody)
	if !ok || groqReq.MaxTokens != nil {
		t.Fatalf("expected nil max_tokens for groq, got %+v", groqReq.MaxTokens)
	}
	groqJSON, err := json.Marshal(groqBody)
	if err != nil {
		t.Fatalf("marshal groqBody: %v", err)
	}
	if strings.Contains(string(groqJSON), `"max_tokens"`) {
		t.Fatalf("expected max_tokens to be omitted in groq body json, got %s", string(groqJSON))
	}

	// Explicit MaxTokens overrides defaults for all providers
	explicit := 512
	reqWithTokens := req
	reqWithTokens.MaxTokens = &explicit

	cfExplicit := buildProviderBody("cloudflare", reqWithTokens).(openAIRequestBody)
	if cfExplicit.MaxTokens == nil || *cfExplicit.MaxTokens != 512 {
		t.Fatalf("expected explicit max_tokens 512 for cloudflare, got %+v", cfExplicit.MaxTokens)
	}

	groqExplicit := buildProviderBody("groq", reqWithTokens).(openAIRequestBody)
	if groqExplicit.MaxTokens == nil || *groqExplicit.MaxTokens != 512 {
		t.Fatalf("expected explicit max_tokens 512 for groq, got %+v", groqExplicit.MaxTokens)
	}
}
