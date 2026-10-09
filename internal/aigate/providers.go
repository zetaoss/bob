package aigate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type ollamaRequestBody struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	Stream      bool      `json:"stream"`
}

type openAIRequestBody struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

type chatCompletionResponse struct {
	ID      string                 `json:"id,omitempty"`
	Object  string                 `json:"object,omitempty"`
	Created int64                  `json:"created,omitempty"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   map[string]any         `json:"usage,omitempty"`
}

type chatCompletionChoice struct {
	Index        int                   `json:"index"`
	FinishReason string                `json:"finish_reason,omitempty"`
	Message      chatCompletionMessage `json:"message"`
}

type chatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatResponse struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	DoneReason string `json:"done_reason"`
	EvalCount  int    `json:"eval_count"`
}

const (
	geminiDefaultEndpoint = "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	ollamaDefaultEndpoint = "https://ollama.com/api/chat"
)

func defaultProviderEndpoint(provider string) string {
	switch strings.ToLower(provider) {
	case "gemini":
		return geminiDefaultEndpoint
	case "ollama":
		return ollamaDefaultEndpoint
	default:
		return ""
	}
}

type providerCallError struct {
	StatusCode int
	Timeout    bool
	Err        error
}

func (e *providerCallError) Error() string {
	if e == nil {
		return ""
	}
	return e.Err.Error()
}

func (g *Gateway) callProvider(route ModelRoute, request ChatCompletionRequest) ([]byte, error) {
	provider, ok := g.providerMap[route.Provider]
	if !ok {
		return nil, fmt.Errorf("provider %q not found", route.Provider)
	}

	endpoint := defaultProviderEndpoint(route.Provider)
	if endpoint == "" {
		return nil, fmt.Errorf("provider %q has no built-in endpoint", route.Provider)
	}

	providerRequest := request
	providerRequest.Model = route.UpstreamModel
	bodyPayload := buildProviderBody(strings.ToLower(route.Provider), providerRequest)
	bodyBytes, err := json.Marshal(bodyPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal provider request: %w", err)
	}
	g.logDebug("provider request provider=%s endpoint=%s body=%s", route.Provider, endpoint, string(bodyBytes))

	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create provider request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if provider.APIKey != "" {
		httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", provider.APIKey))
	}
	g.logDebug("provider request headers provider=%s content-type=%s authorization=%s", route.Provider, httpReq.Header.Get("Content-Type"), redactAuth(httpReq.Header.Get("Authorization")))

	timeout := g.cfg.Fallback.PerAttemptTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		timeoutErr := false
		if nerr, ok := err.(interface{ Timeout() bool }); ok && nerr.Timeout() {
			timeoutErr = true
		}
		if errors.Is(err, context.DeadlineExceeded) {
			timeoutErr = true
		}
		return nil, &providerCallError{
			Timeout: timeoutErr,
			Err:     fmt.Errorf("provider request failed: %w", err),
		}
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read provider response: %w", err)
	}
	g.logDebug("provider response provider=%s status=%d body=%s", route.Provider, resp.StatusCode, string(respBytes))

	if resp.StatusCode >= 400 {
		g.logError("provider error provider=%s status=%d", route.Provider, resp.StatusCode)
		return nil, &providerCallError{
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("provider %s returned status %d: %s", route.Provider, resp.StatusCode, strings.TrimSpace(string(respBytes))),
		}
	}

	return respBytes, nil
}

func redactAuth(authHeader string) string {
	if authHeader == "" {
		return ""
	}
	return "[redacted]"
}

func buildProviderBody(provider string, request ChatCompletionRequest) any {
	switch provider {
	case "gemini":
		return openAIRequestBody{
			Model:       request.Model,
			Messages:    request.Messages,
			Temperature: request.Temperature,
		}
	case "ollama":
		return ollamaRequestBody{
			Model:       request.Model,
			Messages:    request.Messages,
			Temperature: request.Temperature,
			Stream:      false,
		}
	default:
		return openAIRequestBody{
			Model:       request.Model,
			Messages:    request.Messages,
			Temperature: request.Temperature,
		}
	}
}

func normalizeProviderResponse(provider string, publicModel string, responseBody []byte) ([]byte, error) {
	switch provider {
	case "ollama":
		var raw ollamaChatResponse
		if err := json.Unmarshal(responseBody, &raw); err != nil {
			return nil, fmt.Errorf("parse ollama response: %w", err)
		}
		normalized := chatCompletionResponse{
			Object: "chat.completion",
			Model:  publicModel,
			Choices: []chatCompletionChoice{
				{
					Index:        0,
					FinishReason: raw.DoneReason,
					Message: chatCompletionMessage{
						Role:    raw.Message.Role,
						Content: raw.Message.Content,
					},
				},
			},
		}
		if raw.EvalCount > 0 {
			normalized.Usage = map[string]any{
				"completion_tokens": raw.EvalCount,
			}
		}
		return json.Marshal(normalized)
	default:
		var base map[string]any
		if err := json.Unmarshal(responseBody, &base); err != nil {
			return nil, fmt.Errorf("parse provider response: %w", err)
		}
		base["model"] = publicModel
		return json.Marshal(base)
	}
}
