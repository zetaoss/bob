package aigate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestOllamaRequestOptions(t *testing.T) {
	zero, limit := 0.0, 512
	cases := []struct {
		name        string
		temperature *float64
		maxTokens   *int
		want        map[string]any
	}{
		{"omitted", nil, nil, nil},
		{"zero temperature", &zero, nil, map[string]any{"temperature": float64(0)}},
		{"max tokens", nil, &limit, map[string]any{"num_predict": float64(512)}},
		{"both", &zero, &limit, map[string]any{"temperature": float64(0), "num_predict": float64(512)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(buildProviderBody("ollama", ChatCompletionRequest{Model: "test", Temperature: tc.temperature, MaxTokens: tc.maxTokens}))
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if _, ok := got["temperature"]; ok {
				t.Fatalf("unexpected top-level temperature: %s", body)
			}
			if _, ok := got["max_tokens"]; ok {
				t.Fatalf("unexpected top-level max_tokens: %s", body)
			}
			if tc.want == nil {
				if _, ok := got["options"]; ok {
					t.Fatalf("unexpected options: %s", body)
				}
			} else if !reflect.DeepEqual(got["options"], tc.want) {
				t.Fatalf("options = %v, want %v", got["options"], tc.want)
			}
		})
	}
}

func TestNormalizeOllamaResponse(t *testing.T) {
	body, err := normalizeProviderResponse("ollama", "ollama/test", []byte(`{"message":{"role":"assistant","content":"hello"},"done_reason":"stop","eval_count":12}`))
	if err != nil {
		t.Fatal(err)
	}
	var got chatCompletionResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "ollama/test" || len(got.Choices) != 1 || got.Choices[0].Message.Content != "hello" || got.Choices[0].FinishReason != "stop" || got.Usage["completion_tokens"] != float64(12) {
		t.Fatalf("unexpected response: %s", body)
	}
}

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
