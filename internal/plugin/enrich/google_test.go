package enrich

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests pin the fix for the Gemini 2.5 enrichment truncation bug
// (2026-06-16). gemini-2.5-flash is a "thinking" model: by default it spends
// part of the maxOutputTokens budget on internal reasoning before emitting the
// answer, so an entity-rich enrichment was truncated mid-JSON and
// ParseUnifiedResponse rejected it with "invalid unified enrichment JSON".
// The fix sends thinkingConfig.thinkingBudget=0 on every request, which
// disables thinking on flash/flash-lite and hands the whole budget to the
// answer.

// TestGoogleProvider_Complete_DisablesThinking asserts the thinking-disable
// config rides every outbound generateContent request.
func TestGoogleProvider_Complete_DisablesThinking(t *testing.T) {
	var tc *googleThinkingConfig
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req googleGenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		tc = req.GenerationConfig.ThinkingConfig
		json.NewEncoder(w).Encode(googleTextResponse("ok"))
	}))
	defer srv.Close()

	p := NewGoogleLLMProvider()
	p.baseURL = srv.URL
	p.model = "gemini-2.5-flash"
	p.apiKey = "k"

	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if tc == nil {
		t.Fatal("google Complete omitted thinkingConfig; want thinkingBudget=0 to disable Gemini 2.5 thinking")
	}
	if tc.ThinkingBudget != 0 {
		t.Fatalf("google thinkingBudget = %d, want 0 (thinking disabled)", tc.ThinkingBudget)
	}
}

// TestGoogleProvider_DisableThinkingBudgetIsZero pins the disable sentinel and
// the constructor default so a future refactor can't silently turn thinking
// back on and re-introduce the truncation bug.
func TestGoogleProvider_DisableThinkingBudgetIsZero(t *testing.T) {
	if disableThinkingBudget != 0 {
		t.Fatalf("disableThinkingBudget = %d, want 0", disableThinkingBudget)
	}
	if p := NewGoogleLLMProvider(); p.thinkingBudget != disableThinkingBudget {
		t.Fatalf("constructor thinkingBudget = %d, want %d", p.thinkingBudget, disableThinkingBudget)
	}
}
