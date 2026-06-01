package enrich

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests pin the response-token cap threading added so that entity-rich
// enrichments are no longer silently truncated at the old hardcoded 1024. They
// assert, per provider, that the configured cap actually rides the outbound
// request body, that the constructor default protects a direct Complete with no
// Init, and that Init threads the config value (falling back to the default when
// unset).

// TestDefaultEnrichMaxTokens pins the default cap well above the old hardcoded
// 1024 that silently truncated entity-rich enrichments.
func TestDefaultEnrichMaxTokens(t *testing.T) {
	if defaultEnrichMaxTokens != 2048 {
		t.Fatalf("defaultEnrichMaxTokens = %d, want 2048", defaultEnrichMaxTokens)
	}
}

// TestResolveMaxTokens covers the shared fallback helper: a positive configured
// value passes through; zero / negative fall back to the package default.
func TestResolveMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"configured positive", 4096, 4096},
		{"configured one", 1, 1},
		{"zero falls back", 0, defaultEnrichMaxTokens},
		{"negative falls back", -5, defaultEnrichMaxTokens},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveMaxTokens(LLMProviderConfig{MaxTokens: tc.in})
			if got != tc.want {
				t.Fatalf("resolveMaxTokens(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// anthropicTextResponse is the minimal success body the mock servers echo back.
func anthropicTextResponse(text string) anthropicMessagesResponse {
	return anthropicMessagesResponse{
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: text}},
	}
}

func googleTextResponse(text string) googleGenerateResponse {
	resp := googleGenerateResponse{}
	resp.Candidates = []struct {
		Content struct {
			Parts []googlePart `json:"parts"`
		} `json:"content"`
	}{
		{Content: struct {
			Parts []googlePart `json:"parts"`
		}{Parts: []googlePart{{Text: text}}}},
	}
	return resp
}

func openaiTextResponse(text string) openaiChatResponse {
	return openaiChatResponse{
		Choices: []struct {
			Message openaiMessage `json:"message"`
		}{{Message: openaiMessage{Role: "assistant", Content: text}}},
	}
}

// --- Anthropic ---

func TestAnthropicProvider_Complete_CarriesConfiguredMaxTokens(t *testing.T) {
	const want = 4096
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req anthropicMessagesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		got = req.MaxTokens
		json.NewEncoder(w).Encode(anthropicTextResponse("ok"))
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	p.baseURL = srv.URL
	p.model = "claude-haiku"
	p.apiKey = "k"
	p.maxTokens = want

	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if got != want {
		t.Fatalf("anthropic Complete max_tokens = %d, want %d", got, want)
	}
}

// A direct Complete with no Init must still send the constructor default, never
// max_tokens: 0 (which a real API rejects).
func TestAnthropicProvider_Complete_DefaultMaxTokensWithoutInit(t *testing.T) {
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req anthropicMessagesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		got = req.MaxTokens
		json.NewEncoder(w).Encode(anthropicTextResponse("ok"))
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"

	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if got != defaultEnrichMaxTokens {
		t.Fatalf("anthropic Complete default max_tokens = %d, want %d", got, defaultEnrichMaxTokens)
	}
}

func TestAnthropicProvider_SubmitBatch_CarriesConfiguredMaxTokens(t *testing.T) {
	const want = 4096
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/batches" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body anthropicBatchSubmitBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode batch body: %v", err)
		}
		if len(body.Requests) != 1 {
			t.Fatalf("expected 1 batch request, got %d", len(body.Requests))
		}
		got = body.Requests[0].Params.MaxTokens
		json.NewEncoder(w).Encode(map[string]string{
			"id":                "batch_test",
			"processing_status": "in_progress",
		})
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"
	p.maxTokens = want

	id, err := p.SubmitBatch(context.Background(), []BatchItem{{CustomID: "1", System: "s", User: "u"}})
	if err != nil {
		t.Fatalf("SubmitBatch failed: %v", err)
	}
	if id != "batch_test" {
		t.Fatalf("expected batch id 'batch_test', got %q", id)
	}
	if got != want {
		t.Fatalf("anthropic SubmitBatch max_tokens = %d, want %d", got, want)
	}
}

func TestAnthropicProvider_Init_SetsMaxTokensFromConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(anthropicTextResponse("OK"))
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	if err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL:   srv.URL,
		Model:     "m",
		APIKey:    "k",
		MaxTokens: 4096,
	}); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if p.maxTokens != 4096 {
		t.Fatalf("after Init p.maxTokens = %d, want 4096", p.maxTokens)
	}
}

func TestAnthropicProvider_Init_DefaultsMaxTokensWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(anthropicTextResponse("OK"))
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	// MaxTokens left 0 → Init must fall back to the default.
	if err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL: srv.URL,
		Model:   "m",
		APIKey:  "k",
	}); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if p.maxTokens != defaultEnrichMaxTokens {
		t.Fatalf("after Init with unset cap p.maxTokens = %d, want %d", p.maxTokens, defaultEnrichMaxTokens)
	}
}

// --- OpenAI ---

func TestOpenAIProvider_Complete_CarriesConfiguredMaxTokens(t *testing.T) {
	const want = 4096
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openaiChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		got = req.MaxTokens
		json.NewEncoder(w).Encode(openaiTextResponse("ok"))
	}))
	defer srv.Close()

	p := NewOpenAILLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"
	p.maxTokens = want

	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if got != want {
		t.Fatalf("openai Complete max_tokens = %d, want %d", got, want)
	}
}

func TestOpenAIProvider_Init_DefaultsMaxTokensWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(openaiTextResponse(`{"ok":true}`))
	}))
	defer srv.Close()

	p := NewOpenAILLMProvider()
	if err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL: srv.URL,
		Model:   "m",
		APIKey:  "k",
	}); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if p.maxTokens != defaultEnrichMaxTokens {
		t.Fatalf("openai p.maxTokens = %d, want %d", p.maxTokens, defaultEnrichMaxTokens)
	}
}

// --- Google ---

func TestGoogleProvider_Complete_CarriesConfiguredMaxTokens(t *testing.T) {
	const want = 4096
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req googleGenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		got = req.GenerationConfig.MaxOutputTokens
		json.NewEncoder(w).Encode(googleTextResponse("ok"))
	}))
	defer srv.Close()

	p := NewGoogleLLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"
	p.maxTokens = want

	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if got != want {
		t.Fatalf("google Complete maxOutputTokens = %d, want %d", got, want)
	}
}

func TestGoogleProvider_Init_DefaultsMaxTokensWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(googleTextResponse(`{"ok":true}`))
	}))
	defer srv.Close()

	p := NewGoogleLLMProvider()
	if err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL: srv.URL,
		Model:   "m",
		APIKey:  "k",
	}); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if p.maxTokens != defaultEnrichMaxTokens {
		t.Fatalf("google p.maxTokens = %d, want %d", p.maxTokens, defaultEnrichMaxTokens)
	}
}
