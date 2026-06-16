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

// disableThinkingBudget turns Gemini 2.5 "thinking" OFF, so the whole
// maxOutputTokens budget goes to the actual answer. See googleThinkingConfig
// for why enrichment requires this.
const disableThinkingBudget = 0

// GoogleLLMProvider is an HTTP client for Google's Gemini generateContent endpoint.
type GoogleLLMProvider struct {
	client  *http.Client
	baseURL string
	model   string
	apiKey  string

	// maxTokens caps the response tokens on every request (maxOutputTokens).
	// Set from config in Init; defaulted in the constructor so a direct
	// Complete without Init never serialises maxOutputTokens: 0.
	maxTokens int

	// thinkingBudget caps Gemini 2.5 "thinking" tokens, which count against
	// maxOutputTokens. Defaulted to disableThinkingBudget (0) in the
	// constructor because enrichment never benefits from thinking — see
	// googleThinkingConfig.
	thinkingBudget int
}

// googleGenerateRequest is the request structure for Gemini generateContent.
type googleGenerateRequest struct {
	Contents          []googleContent       `json:"contents"`
	SystemInstruction *googleSystemContent  `json:"systemInstruction,omitempty"`
	GenerationConfig  googleGenerationConfig `json:"generationConfig"`
}

type googleContent struct {
	Role  string       `json:"role"`
	Parts []googlePart `json:"parts"`
}

type googleSystemContent struct {
	Parts []googlePart `json:"parts"`
}

type googlePart struct {
	Text string `json:"text"`
}

type googleGenerationConfig struct {
	Temperature      float32               `json:"temperature"`
	MaxOutputTokens  int                   `json:"maxOutputTokens"`
	ResponseMimeType string                `json:"responseMimeType"`
	ThinkingConfig   *googleThinkingConfig `json:"thinkingConfig,omitempty"`
}

// googleThinkingConfig controls Gemini 2.5 "thinking". For enrichment — a
// mechanical entity/JSON extraction task — thinking is pure waste: the thinking
// tokens are drawn from the maxOutputTokens budget BEFORE the answer is emitted,
// so an entity-rich enrichment gets truncated mid-JSON and ParseUnifiedResponse
// rejects it with "invalid unified enrichment JSON". Proven live 2026-06-16:
// gemini-2.5-flash burned 1569 of 2048 tokens thinking (finishReason MAX_TOKENS,
// JSON cut off mid-array); the identical call with thinkingBudget=0 finished
// clean (finishReason STOP, full budget to the answer, valid JSON). A budget of
// 0 disables thinking on gemini-2.5-flash and -flash-lite. (gemini-2.5-pro
// cannot fully disable thinking — if enrichment ever moves to pro, set this
// above pro's minimum rather than 0.)
type googleThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

// googleGenerateResponse is the response structure from Gemini generateContent.
type googleGenerateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []googlePart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// NewGoogleLLMProvider creates a new Google Gemini provider.
func NewGoogleLLMProvider() *GoogleLLMProvider {
	return &GoogleLLMProvider{
		client: &http.Client{
			Timeout:   300 * time.Second,
			Transport: plugin.WrapTransport(nil),
		},
		maxTokens:      defaultEnrichMaxTokens,
		thinkingBudget: disableThinkingBudget,
	}
}

// Name returns the provider name.
func (p *GoogleLLMProvider) Name() string {
	return "google"
}

// Init initializes the provider and validates connectivity.
func (p *GoogleLLMProvider) Init(ctx context.Context, cfg LLMProviderConfig) error {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model
	p.apiKey = cfg.APIKey
	p.maxTokens = resolveMaxTokens(cfg)

	if p.apiKey == "" {
		return fmt.Errorf("google provider requires API key")
	}

	// Send a probe completion request to validate connectivity.
	// The system prompt explicitly mentions "json" to be consistent with the
	// OpenAI provider pattern — defensively guards against providers that
	// reject JSON output mode without a json keyword in the prompt.
	_, err := p.Complete(ctx, "You are a connectivity probe. Respond with valid JSON only.", `{"ok":true}`)
	if err != nil {
		return fmt.Errorf("google connectivity check failed: %w", err)
	}

	return nil
}

// Complete sends a generateContent request to the Gemini API.
func (p *GoogleLLMProvider) Complete(ctx context.Context, system, user string) (string, error) {
	req := googleGenerateRequest{
		Contents: []googleContent{
			{Role: "user", Parts: []googlePart{{Text: user}}},
		},
		SystemInstruction: &googleSystemContent{
			Parts: []googlePart{{Text: system}},
		},
		GenerationConfig: googleGenerationConfig{
			Temperature:      0.0,
			MaxOutputTokens:  p.maxTokens,
			ResponseMimeType: "application/json",
			ThinkingConfig:   &googleThinkingConfig{ThinkingBudget: p.thinkingBudget},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", p.baseURL, p.model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	// Google uses x-goog-api-key, not Authorization: Bearer.
	httpReq.Header.Set("x-goog-api-key", p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("google returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var genResp googleGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&genResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(genResp.Candidates) == 0 || len(genResp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("google response has no candidates")
	}

	return genResp.Candidates[0].Content.Parts[0].Text, nil
}

// Close releases HTTP connections.
func (p *GoogleLLMProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
