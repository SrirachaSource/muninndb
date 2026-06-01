package enrich

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

func TestDetectContentType(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want ContentType
	}{
		{"empty", "", ContentPlain},
		{"whitespace only", "   \n\t  ", ContentPlain},
		{"prose", "We rotated into financials on Friday after the COT print.", ContentPlain},
		{"json object", `{"host":"db","port":5432,"tags":["a","b"]}`, ContentJSON},
		{"json array", `[{"id":1},{"id":2}]`, ContentJSON},
		{"loose json trailing comma", `{"a":1,"b":2,}`, ContentJSON},
		{"go func signature", "func Add(a, b int) int { return a + b }", ContentCode},
		{"python def", "def add(a, b):\n    return a + b", ContentCode},
		{"unified diff hunk", "@@ -1,3 +1,4 @@\n-old\n+new\n context", ContentCode},
		{"git diff header", "diff --git a/x.go b/x.go\nindex 111..222 100644", ContentCode},
		{"fenced code whole body", "```go\nfunc x() {}\n```", ContentCode},
		{"prose then fenced code", "Here is the fix:\n```go\nfunc x() {}\n```", ContentMixed},
		{"fence with trailing prose", "```\nx = 1\n```\nThanks.", ContentMixed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectContentType(tc.in); got != tc.want {
				t.Fatalf("DetectContentType(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestContentHint(t *testing.T) {
	// Regression guard: the dominant prose path must carry NO hint, so the
	// user message stays byte-identical to the legacy format.
	if got := contentHint(ContentPlain); got != "" {
		t.Fatalf("contentHint(ContentPlain) = %q, want empty", got)
	}
	if h := contentHint(ContentJSON); !strings.Contains(h, "JSON object") {
		t.Fatalf("json hint missing JSON guidance: %q", h)
	}
	if h := contentHint(ContentCode); !strings.Contains(h, "source code") {
		t.Fatalf("code hint missing code guidance: %q", h)
	}
	if h := contentHint(ContentMixed); !strings.Contains(h, "JSON object") || !strings.Contains(h, "source code") {
		t.Fatalf("mixed hint should carry both: %q", h)
	}
}

func TestBuildUserContent(t *testing.T) {
	// Plain prose -> byte-identical to the legacy fmt.Sprintf format, no hint.
	plain := buildUserContent(&storage.Engram{Concept: "c", Content: "just prose"})
	if want := "Concept: c\n\nContent: just prose"; plain != want {
		t.Fatalf("plain user message not legacy-identical:\n got %q\nwant %q", plain, want)
	}

	// JSON -> json hint prefix, then the legacy block intact.
	jsonMsg := buildUserContent(&storage.Engram{Concept: "c", Content: `{"a":1}`})
	if !strings.HasPrefix(jsonMsg, jsonHint+"\n\n") {
		t.Fatalf("json user message missing hint prefix: %q", jsonMsg)
	}
	if !strings.Contains(jsonMsg, `Concept: c`+"\n\n"+`Content: {"a":1}`) {
		t.Fatalf("json user message missing concept/content block: %q", jsonMsg)
	}

	// Code -> code hint prefix.
	codeMsg := buildUserContent(&storage.Engram{Concept: "c", Content: "func x() {}"})
	if !strings.HasPrefix(codeMsg, codeHint+"\n\n") {
		t.Fatalf("code user message missing hint prefix: %q", codeMsg)
	}
}

// unifiedMockResponse is a complete unified-enrichment JSON the mock returns so
// pipeline.Run parses and succeeds for an all-stages engram.
const unifiedMockResponse = `{
	"entities": [{"name": "X", "type": "concept", "confidence": 0.9}],
	"relationships": [],
	"memory_type": "fact",
	"type_label": "",
	"category": "c",
	"subcategory": "s",
	"tags": ["t"],
	"summary": "a summary",
	"key_points": ["k1", "k2", "k3"]
}`

// TestEnrich_UserCarriesHint_SystemPromptStable is the cache guard: across a JSON
// engram and a plain engram with the same enabled stages, the SYSTEM prompt must
// be byte-identical (so Anthropic's prompt cache holds), while the USER message
// differs -- the JSON engram carries the content-type hint and the plain one does
// not (and stays legacy-identical).
func TestEnrich_UserCarriesHint_SystemPromptStable(t *testing.T) {
	limiter := NewTokenBucketLimiter(100.0, 100.0)

	run := func(eng *storage.Engram) (system, user string) {
		m := NewMockLLMProvider()
		m.customComplete = func(_ context.Context, sys, usr string) (string, error) {
			system, user = sys, usr
			return unifiedMockResponse, nil
		}
		p := NewPipeline(m, limiter)
		if _, err := p.Run(context.Background(), eng); err != nil {
			t.Fatalf("pipeline.Run failed: %v", err)
		}
		return system, user
	}

	sysJSON, usrJSON := run(&storage.Engram{ID: storage.NewULID(), Concept: "cfg", Content: `{"host":"db","port":5432}`})
	sysPlain, usrPlain := run(&storage.Engram{ID: storage.NewULID(), Concept: "note", Content: "we shipped the release today"})

	// Cache guard: same stages => identical system prompt regardless of content.
	if sysJSON != sysPlain {
		t.Fatal("system prompt differs by content type -- the prompt cache would break")
	}

	// JSON user message carries the hint; plain does not.
	if !strings.Contains(usrJSON, jsonHint) {
		t.Fatalf("json user message missing hint:\n%s", usrJSON)
	}
	if strings.Contains(usrPlain, jsonHint) || strings.Contains(usrPlain, codeHint) {
		t.Fatalf("plain user message unexpectedly carries a hint:\n%s", usrPlain)
	}

	// Plain path stays byte-identical to the legacy format.
	if want := "Concept: note\n\nContent: we shipped the release today"; usrPlain != want {
		t.Fatalf("plain user message not legacy-identical:\n got %q\nwant %q", usrPlain, want)
	}
}
