package enrich

import (
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
)

// TestParseUnifiedResponse_AllFields verifies the unified parser populates
// every requested field from a complete LLM response.
func TestParseUnifiedResponse_AllFields(t *testing.T) {
	raw := `{
		"entities": [{"name": "PostgreSQL", "type": "database", "confidence": 0.95}],
		"relationships": [{"from": "PostgreSQL", "to": "PostgreSQL", "type": "alternative_to", "weight": 0.5}],
		"memory_type": "decision",
		"type_label": "architectural_decision",
		"category": "infrastructure",
		"subcategory": "databases",
		"tags": ["db", "postgres"],
		"summary": "Picked PostgreSQL for ACID guarantees.",
		"key_points": ["ACID matters", "no SQL fallback"]
	}`
	enabled := []string{"entities", "relationships", "classification", "summary"}

	res, err := ParseUnifiedResponse(raw, enabled)
	if err != nil {
		t.Fatalf("ParseUnifiedResponse: %v", err)
	}
	if len(res.Entities) != 1 || res.Entities[0].Name != "PostgreSQL" {
		t.Fatalf("unexpected entities: %+v", res.Entities)
	}
	if len(res.Relationships) != 1 || res.Relationships[0].FromEntity != "PostgreSQL" {
		t.Fatalf("unexpected relationships: %+v", res.Relationships)
	}
	if res.MemoryType != "decision" || res.TypeLabel != "architectural_decision" {
		t.Fatalf("unexpected classification: %+v", res)
	}
	if res.Category != "infrastructure" || res.Subcategory != "databases" {
		t.Fatalf("unexpected category/subcategory: %s/%s", res.Category, res.Subcategory)
	}
	if len(res.Tags) != 2 {
		t.Fatalf("unexpected tags: %v", res.Tags)
	}
	if res.Summary == "" || len(res.KeyPoints) != 2 {
		t.Fatalf("unexpected summary/key_points: %q / %v", res.Summary, res.KeyPoints)
	}
}

// TestParseUnifiedResponse_OmitsDisabledFields verifies that fields produced
// by the LLM for stages NOT in the enabled list are dropped.
func TestParseUnifiedResponse_OmitsDisabledFields(t *testing.T) {
	raw := `{
		"entities": [{"name": "foo", "type": "tool", "confidence": 1.0}],
		"summary": "should not appear",
		"key_points": ["x"]
	}`
	res, err := ParseUnifiedResponse(raw, []string{"entities"})
	if err != nil {
		t.Fatalf("ParseUnifiedResponse: %v", err)
	}
	if res.Summary != "" || len(res.KeyPoints) != 0 {
		t.Fatalf("expected summary/key_points dropped when stage disabled, got %q / %v", res.Summary, res.KeyPoints)
	}
	if len(res.Entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(res.Entities))
	}
}

// TestParseUnifiedResponse_DropsHallucinatedRelationships covers the safety
// filter: relationships whose endpoints aren't in the extracted entities are
// dropped.
func TestParseUnifiedResponse_DropsHallucinatedRelationships(t *testing.T) {
	raw := `{
		"entities": [{"name": "A", "type": "service", "confidence": 1.0}],
		"relationships": [
			{"from": "A", "to": "GhostX", "type": "uses", "weight": 0.9},
			{"from": "Ghost1", "to": "Ghost2", "type": "uses", "weight": 0.8}
		]
	}`
	res, err := ParseUnifiedResponse(raw, []string{"entities", "relationships"})
	if err != nil {
		t.Fatalf("ParseUnifiedResponse: %v", err)
	}
	if len(res.Relationships) != 0 {
		t.Fatalf("expected hallucinated relationships dropped, got %+v", res.Relationships)
	}
}

// TestParseUnifiedResponse_PartialJSON verifies that missing optional fields
// produce a partial result rather than an error.
func TestParseUnifiedResponse_PartialJSON(t *testing.T) {
	raw := `{"summary": "only a summary", "key_points": ["a"]}`
	res, err := ParseUnifiedResponse(raw, []string{"entities", "summary"})
	if err != nil {
		t.Fatalf("ParseUnifiedResponse: %v", err)
	}
	if res.Summary != "only a summary" {
		t.Fatalf("unexpected summary: %q", res.Summary)
	}
	if len(res.Entities) != 0 {
		t.Fatalf("expected 0 entities when LLM omitted the field, got %d", len(res.Entities))
	}
}

// TestParseUnifiedResponse_AllEmpty returns an error so the pipeline can
// distinguish "model returned nothing usable" from "model returned a partial
// result".
func TestParseUnifiedResponse_AllEmpty(t *testing.T) {
	raw := `{}`
	res, err := ParseUnifiedResponse(raw, []string{"entities", "summary"})
	if err == nil {
		t.Fatalf("expected error for all-empty response, got %+v", res)
	}
}

// TestParseUnifiedResponse_BadJSON returns an error.
func TestParseUnifiedResponse_BadJSON(t *testing.T) {
	_, err := ParseUnifiedResponse(`not valid json`, []string{"summary"})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// TestParseUnifiedResponse_MarkdownFenced verifies that JSON wrapped in
// markdown code fences is still parsed.
func TestParseUnifiedResponse_MarkdownFenced(t *testing.T) {
	raw := "```json\n{\"summary\":\"fenced\",\"key_points\":[\"a\"]}\n```"
	res, err := ParseUnifiedResponse(raw, []string{"summary"})
	if err != nil {
		t.Fatalf("ParseUnifiedResponse: %v", err)
	}
	if res.Summary != "fenced" {
		t.Fatalf("expected 'fenced', got %q", res.Summary)
	}
}

// TestExtractJSON_WithPreamble tests JSON extraction from text with preamble.
func TestExtractJSON_WithPreamble(t *testing.T) {
	raw := `Here is the JSON response:
{"test": "value"}`
	extracted := extractJSON(raw)

	if !contains(extracted, `"test"`) || !contains(extracted, `"value"`) {
		t.Fatalf("Failed to extract JSON from preamble text: %q", extracted)
	}
}

// TestExtractJSON_WithMarkdownFences tests JSON extraction from markdown fences.
func TestExtractJSON_WithMarkdownFences(t *testing.T) {
	raw := "```json\n{\"test\": \"value\"}\n```"
	extracted := extractJSON(raw)

	if !contains(extracted, `"test"`) || !contains(extracted, `"value"`) {
		t.Fatalf("Failed to extract JSON from markdown fences: %q", extracted)
	}
}

// TestNormalizeEntityType tests entity type normalization and validation.
func TestNormalizeEntityType_Valid(t *testing.T) {
	tests := map[string]string{
		// Known types — returned as-is after normalisation.
		"person":       "person",
		"PERSON":       "person",
		"database":     "database",
		"tool":         "tool",
		"ORGANIZATION": "organization",
		// UI-colour-map types that were previously missing from the allowlist
		// and were silently coerced to "service".
		"technology": "technology",
		"location":   "location",
		"concept":    "concept",
		"product":    "product",
		"event":      "event",
		// Unknown types are passed through (not coerced to "service").
		"unknown": "unknown",
		"library": "library",
		"LIBRARY": "library", // still normalised to lowercase
	}

	for input, expected := range tests {
		result := normalizeEntityType(input)
		if result != expected {
			t.Errorf("normalizeEntityType(%q): got %q, want %q", input, result, expected)
		}
	}
}

// TestNormalizeEntityType_UnknownPassThrough verifies that unknown entity types
// are returned as their normalised string rather than silently coerced to
// "service". This prevents data corruption when an LLM returns a valid semantic
// type (e.g. "library", "concept", "event") that is not yet in the allowlist.
func TestNormalizeEntityType_UnknownPassThrough(t *testing.T) {
	unknownTypes := []string{"library", "algorithm", "protocol", "api", "config", "file"}
	for _, typ := range unknownTypes {
		result := normalizeEntityType(typ)
		if result == "service" {
			t.Errorf("normalizeEntityType(%q) = %q, must not coerce unknown types to \"service\"", typ, result)
		}
		if result != typ {
			t.Errorf("normalizeEntityType(%q) = %q, want pass-through %q", typ, result, typ)
		}
	}
}

// TestValidateAndDedupeEntities tests deduplication, empty names, and confidence clamping.
func TestValidateAndDedupeEntities(t *testing.T) {
	input := []plugin.ExtractedEntity{
		{Name: "PostgreSQL", Type: "database", Confidence: 0.8},
		{Name: "PostgreSQL", Type: "database", Confidence: 0.95}, // higher confidence wins
		{Name: "Redis", Type: "tool", Confidence: 0.7},
		{Name: "", Type: "tool", Confidence: 0.5},       // empty name => skipped
		{Name: "Neg", Type: "person", Confidence: -0.5}, // clamped to 0.0
		{Name: "Over", Type: "person", Confidence: 1.5}, // clamped to 1.0
	}

	result := validateAndDedupeEntities(input)

	byName := map[string]plugin.ExtractedEntity{}
	for _, e := range result {
		byName[e.Name] = e
	}

	if _, ok := byName[""]; ok {
		t.Fatal("empty-name entity should have been removed")
	}

	pg, ok := byName["PostgreSQL"]
	if !ok {
		t.Fatal("PostgreSQL missing")
	}
	if pg.Confidence != 0.95 {
		t.Fatalf("expected PostgreSQL confidence 0.95, got %v", pg.Confidence)
	}

	neg := byName["Neg"]
	if neg.Confidence != 0.0 {
		t.Fatalf("expected clamped confidence 0.0, got %v", neg.Confidence)
	}
	over := byName["Over"]
	if over.Confidence != 1.0 {
		t.Fatalf("expected clamped confidence 1.0, got %v", over.Confidence)
	}
}

func TestValidateRelationships(t *testing.T) {
	input := []plugin.ExtractedRelation{
		{FromEntity: "A", ToEntity: "B", RelType: "uses", Weight: 0.9},
		{FromEntity: "", ToEntity: "B", RelType: "uses", Weight: 0.5},   // empty from => skip
		{FromEntity: "A", ToEntity: "", RelType: "uses", Weight: 0.5},   // empty to => skip
		{FromEntity: "C", ToEntity: "D", RelType: "uses", Weight: -0.3}, // clamped to 0.0
		{FromEntity: "E", ToEntity: "F", RelType: "uses", Weight: 1.5},  // clamped to 1.0
	}

	result := validateRelationships(input)

	if len(result) != 3 {
		t.Fatalf("expected 3 valid relationships, got %d", len(result))
	}

	if result[1].Weight != 0.0 {
		t.Fatalf("expected clamped weight 0.0, got %v", result[1].Weight)
	}
	if result[2].Weight != 1.0 {
		t.Fatalf("expected clamped weight 1.0, got %v", result[2].Weight)
	}
}

func TestExtractJSON_PlainCodeFences(t *testing.T) {
	raw := "```\n{\"key\": \"val\"}\n```"
	extracted := extractJSON(raw)
	if !contains(extracted, `"key"`) {
		t.Fatalf("failed to extract from plain code fences: %q", extracted)
	}
}

// TestExtractJSON_DuplicateOutput covers models (e.g. llama3.2) that repeat
// their JSON output in a single completion. The parser must return only the
// first complete object and ignore everything after it.
func TestExtractJSON_DuplicateOutput(t *testing.T) {
	raw := `{"entities": [{"name": "foo", "type": "tool"}]} {"entities": [{"name": "bar", "type": "tool"}]}`
	extracted := extractJSON(raw)
	// Must stop at the end of the first object — second object must not appear.
	if contains(extracted, `"bar"`) {
		t.Fatalf("extractJSON grabbed both duplicate objects: %q", extracted)
	}
	if !contains(extracted, `"foo"`) {
		t.Fatalf("extractJSON dropped the first object: %q", extracted)
	}
}

// TestParseUnifiedResponse_DuplicateOutput ensures the unified parser handles
// models that repeat their JSON output (e.g. llama3.2 quirk): only the first
// object's fields appear.
func TestParseUnifiedResponse_DuplicateOutput(t *testing.T) {
	raw := `{"summary": "first", "key_points": ["A"]} {"summary": "second", "key_points": ["B"]}`
	res, err := ParseUnifiedResponse(raw, []string{"summary"})
	if err != nil {
		t.Fatalf("ParseUnifiedResponse: %v", err)
	}
	if res.Summary != "first" {
		t.Fatalf("expected 'first', got %q", res.Summary)
	}
	if len(res.KeyPoints) != 1 || res.KeyPoints[0] != "A" {
		t.Fatalf("expected ['A'], got %v", res.KeyPoints)
	}
}

// TestExtractJSON_BracketInsideString ensures brackets inside quoted strings
// do not confuse the depth counter.
func TestExtractJSON_BracketInsideString(t *testing.T) {
	raw := `{"key": "value with } brace and { another"}`
	extracted := extractJSON(raw)
	if extracted != raw {
		t.Fatalf("extractJSON mangled JSON with brackets in string: %q", extracted)
	}
}

func TestExtractJSON_NoJSON(t *testing.T) {
	raw := "no json here"
	extracted := extractJSON(raw)
	if extracted != "no json here" {
		t.Fatalf("expected raw string back, got %q", extracted)
	}
}

func TestExtractJSON_ArrayBrackets(t *testing.T) {
	raw := `some text [{"a":1}]`
	extracted := extractJSON(raw)
	if !contains(extracted, `[{"a":1}]`) {
		t.Fatalf("failed to extract array: %q", extracted)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
