package aigate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"bob/internal/config"
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

// Variables so tests can point them at a local server.
var (
	geminiDefaultEndpoint = "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
	groqDefaultEndpoint   = "https://api.groq.com/openai/v1/chat/completions"
	ollamaDefaultEndpoint = "https://ollama.com/api/chat"
	// cloudflareAPIBase is joined with /accounts/{accountId}/ai/... for Workers AI.
	cloudflareAPIBase = "https://api.cloudflare.com/client/v4"
)

func providerEndpoint(provider string, cfg config.ProviderConfig) string {
	switch strings.ToLower(provider) {
	case "cloudflare":
		return cloudflareAPIBase + "/accounts/" + url.PathEscape(cfg.AccountID) + "/ai/v1/chat/completions"
	case "gemini":
		return geminiDefaultEndpoint
	case "groq":
		return groqDefaultEndpoint
	case "ollama":
		return ollamaDefaultEndpoint
	default:
		return ""
	}
}

type providerCallError struct {
	StatusCode int
	Timeout    bool
	Body       []byte
	Header     http.Header
	Err        error
}

func (e *providerCallError) Error() string {
	if e == nil {
		return ""
	}
	return e.Err.Error()
}

// callProvider returns the response body and headers; headers carry provider rate limit state (Groq).
func (g *Gateway) callProvider(route ModelRoute, request ChatCompletionRequest) ([]byte, http.Header, error) {
	provider, ok := g.providerMap[route.Provider]
	if !ok {
		return nil, nil, fmt.Errorf("provider %q not found", route.Provider)
	}

	endpoint := providerEndpoint(route.Provider, provider)
	if endpoint == "" {
		return nil, nil, fmt.Errorf("provider %q has no built-in endpoint", route.Provider)
	}

	providerRequest := request
	providerRequest.Model = route.UpstreamModel
	bodyPayload := buildProviderBody(strings.ToLower(route.Provider), providerRequest)
	bodyBytes, err := json.Marshal(bodyPayload)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal provider request: %w", err)
	}
	g.log.Debug("provider request", "provider", route.Provider, "endpoint", endpoint, "body", string(bodyBytes))

	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("create provider request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if provider.APIKey != "" {
		httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", provider.APIKey))
	}
	g.log.Debug("provider request headers", "provider", route.Provider, "content_type", httpReq.Header.Get("Content-Type"), "authorization", redactAuth(httpReq.Header.Get("Authorization")))

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
		// The request URL can hold an account ID (Cloudflare) and ends up in callers' run records, so only
		// the log keeps it.
		g.log.Error("provider request failed", "provider", route.Provider, "err", err)
		return nil, nil, &providerCallError{
			Timeout: timeoutErr,
			Err:     transportError(route.Provider, timeout, timeoutErr, err),
		}
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read provider response: %w", err)
	}
	g.log.Debug("provider response", "provider", route.Provider, "status", resp.StatusCode, "body", string(respBytes))

	if resp.StatusCode >= 400 {
		// The whole body stays in the log; callers get providerMessage.
		g.log.Error("provider error", "provider", route.Provider, "status", resp.StatusCode, "body", truncate(string(respBytes), 2000))
		return nil, nil, &providerCallError{
			StatusCode: resp.StatusCode,
			Body:       respBytes,
			Header:     resp.Header,
			Err:        fmt.Errorf("provider %s returned status %d: %s", route.Provider, resp.StatusCode, providerMessage(respBytes)),
		}
	}

	return respBytes, resp.Header, nil
}

var (
	// IDs providers put in messages, Groq organizations (with Groq's "in organization `org_…`" phrase) and
	// Cloudflare accounts in API paths, and URLs and Groq's upsell that add nothing.
	orgIDPattern       = regexp.MustCompile("org_[A-Za-z0-9]+")
	accountPathPattern = regexp.MustCompile(`/accounts/[A-Za-z0-9]+`)
	inOrgPattern       = regexp.MustCompile(" in organization `[^`]*`")
	urlPattern         = regexp.MustCompile(`https?://\S+`)
	upsellPattern      = regexp.MustCompile(`\s*Need more tokens\?.*$`)
)

const maxProviderMessage = 300

// providerMessage is the message of a provider's error response for callers, which keep it in run records and
// show it: the error message out of the JSON (an error object, a list of them, or Cloudflare's errors), without
// account or organization IDs and URLs, or the trimmed body when it is not JSON. The usage tracker reads the
// body itself, and the log keeps it whole.
func providerMessage(body []byte) string {
	msg := strings.TrimSpace(string(body))
	type errObj struct {
		Error any `json:"error"`
	}
	var one errObj
	var list []errObj
	var cf struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	switch {
	case json.Unmarshal(body, &one) == nil && one.Error != nil:
		msg = errorText(one.Error, msg)
	case json.Unmarshal(body, &list) == nil && len(list) > 0 && list[0].Error != nil:
		msg = errorText(list[0].Error, msg)
	case json.Unmarshal(body, &cf) == nil && len(cf.Errors) > 0 && cf.Errors[0].Message != "":
		msg = cf.Errors[0].Message
	}
	msg = inOrgPattern.ReplaceAllString(msg, "")
	msg = orgIDPattern.ReplaceAllString(msg, "org_…")
	msg = accountPathPattern.ReplaceAllString(msg, "/accounts/…")
	msg = upsellPattern.ReplaceAllString(msg, "")
	msg = urlPattern.ReplaceAllString(msg, "<url>")
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); len(r) > maxProviderMessage {
		msg = string(r[:maxProviderMessage]) + "…"
	}
	return msg
}

// errorText is the message of a JSON "error" value: a string, or an object with a message.
func errorText(v any, fallback string) string {
	switch e := v.(type) {
	case string:
		return e
	case map[string]any:
		if m, ok := e["message"].(string); ok && m != "" {
			return m
		}
	}
	return fallback
}

// transportError describes a provider request that got no response without its URL: a timeout, or the
// cause inside the *url.Error.
func transportError(provider string, timeout time.Duration, timedOut bool, err error) error {
	if timedOut {
		return fmt.Errorf("provider %s timed out after %s", provider, timeout)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return fmt.Errorf("provider %s request failed: %w", provider, err)
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
