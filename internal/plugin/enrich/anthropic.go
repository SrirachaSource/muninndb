package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
)

// AnthropicLLMProvider is an HTTP client for Anthropic's /v1/messages endpoint.
// The system prompt is sent as a single cacheable content block so identical
// system prompts across many calls hit Anthropic's prompt cache (90% input
// discount on cache hits). The 1h cache TTL is opted into via the
// extended-cache-ttl-2025-04-11 beta header.
type AnthropicLLMProvider struct {
	client  *http.Client
	baseURL string
	model   string
	apiKey  string

	// maxTokens caps the response tokens on every request. The sync Complete
	// path and the SubmitBatch path share this one field (same struct). Set
	// from config in Init; defaulted in the constructor so a direct Complete
	// without Init never serialises max_tokens: 0.
	maxTokens int
}

// anthropicCacheControl marks a content block as cacheable. type is always
// "ephemeral"; ttl defaults to "5m" when omitted by Anthropic, or "1h" when
// the extended-cache-ttl-2025-04-11 beta header is on the request.
type anthropicCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// anthropicSystemBlock is a single content block in the system field. We
// always send the system as a one-element array of these so cache_control
// can attach.
type anthropicSystemBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicMessagesRequest is the request structure for Anthropic messages API.
type anthropicMessagesRequest struct {
	Model     string                 `json:"model"`
	MaxTokens int                    `json:"max_tokens"`
	System    []anthropicSystemBlock `json:"system"`
	Messages  []anthropicMessage     `json:"messages"`
}

// anthropicMessage is a message in the Anthropic messages API.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicMessagesResponse is the response structure from Anthropic messages API.
type anthropicMessagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// NewAnthropicLLMProvider creates a new Anthropic provider.
func NewAnthropicLLMProvider() *AnthropicLLMProvider {
	return &AnthropicLLMProvider{
		client: &http.Client{
			Timeout:   300 * time.Second,
			Transport: plugin.WrapTransport(nil),
		},
		maxTokens: defaultEnrichMaxTokens,
	}
}

// Name returns the provider name.
func (p *AnthropicLLMProvider) Name() string {
	return "anthropic"
}

// Init initializes the provider and validates connectivity.
func (p *AnthropicLLMProvider) Init(ctx context.Context, cfg LLMProviderConfig) error {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model
	p.apiKey = cfg.APIKey
	p.maxTokens = resolveMaxTokens(cfg)

	if p.apiKey == "" {
		return fmt.Errorf("anthropic provider requires API key")
	}

	// Send a probe completion request to validate connectivity.
	_, err := p.Complete(ctx, "You are a helpful assistant.", "Say 'OK' only.")
	if err != nil {
		return fmt.Errorf("anthropic connectivity check failed: %w", err)
	}

	return nil
}

// Complete sends a messages request to Anthropic. The system prompt is wrapped
// in a single cacheable content block (cache_control: ephemeral, ttl=1h) so
// repeated calls with the same system prompt hit Anthropic's prompt cache.
func (p *AnthropicLLMProvider) Complete(ctx context.Context, system, user string) (string, error) {
	req := anthropicMessagesRequest{
		Model:     p.model,
		MaxTokens: p.maxTokens,
		System: []anthropicSystemBlock{
			{
				Type:         "text",
				Text:         system,
				CacheControl: &anthropicCacheControl{Type: "ephemeral", TTL: "1h"},
			},
		},
		Messages: []anthropicMessage{
			{Role: "user", Content: user},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		"POST",
		p.baseURL+"/v1/messages",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	// Opt into 1h cache TTL. Without this header, ttl="1h" on the cache
	// control block is rejected by the API.
	httpReq.Header.Set("anthropic-beta", "extended-cache-ttl-2025-04-11")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("anthropic returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var messagesResp anthropicMessagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&messagesResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(messagesResp.Content) == 0 {
		return "", fmt.Errorf("anthropic response has no content")
	}

	// Return the first text block.
	for _, block := range messagesResp.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}

	return "", fmt.Errorf("anthropic response has no text blocks")
}

// Close releases HTTP connections.
func (p *AnthropicLLMProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
