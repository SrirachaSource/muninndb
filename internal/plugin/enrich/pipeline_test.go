package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/storage"
)

// MockLLMProvider is a mock LLM provider for testing. The default Complete
// recognises the unified enrichment prompt (the only system prompt the
// pipeline now emits) and returns a complete JSON response containing all
// stage fields; ParseUnifiedResponse drops the fields for stages that aren't
// in the pipeline's enabled set.
type MockLLMProvider struct {
	responses      map[string]string
	callCount      int
	failCount      int
	entityResponse string
	customComplete func(ctx context.Context, system, user string) (string, error)
}

func NewMockLLMProvider() *MockLLMProvider {
	return &MockLLMProvider{
		responses: make(map[string]string),
	}
}

func (m *MockLLMProvider) Name() string { return "mock" }

func (m *MockLLMProvider) Init(ctx context.Context, cfg LLMProviderConfig) error {
	return nil
}

func (m *MockLLMProvider) Complete(ctx context.Context, system, user string) (string, error) {
	if m.customComplete != nil {
		return m.customComplete(ctx, system, user)
	}

	m.callCount++

	if m.failCount > 0 {
		m.failCount--
		return "", fmt.Errorf("mock provider error")
	}

	if contains(system, "memory enrichment system") {
		entities := `[{"name": "PostgreSQL", "type": "database", "confidence": 0.95}]`
		if m.entityResponse != "" {
			if extracted := extractEntitiesArrayJSON(m.entityResponse); extracted != "" {
				entities = extracted
			}
		}
		return `{
			"entities": ` + entities + `,
			"relationships": [{"from": "PostgreSQL", "to": "PostgreSQL", "type": "alternative_to", "weight": 0.5}],
			"memory_type": "decision",
			"type_label": "",
			"category": "infrastructure",
			"subcategory": "databases",
			"tags": ["db"],
			"summary": "This is a test summary.",
			"key_points": ["point 1", "point 2"]
		}`, nil
	}

	return "{}", nil
}

func (m *MockLLMProvider) Close() error { return nil }

// extractEntitiesArrayJSON pulls the value of a top-level "entities" field
// from a JSON string, returning just the array JSON. Returns "" if the field
// is absent or the input is invalid.
func extractEntitiesArrayJSON(jsonStr string) string {
	var wrapper struct {
		Entities json.RawMessage `json:"entities"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
		return ""
	}
	if len(wrapper.Entities) == 0 {
		return ""
	}
	return string(wrapper.Entities)
}

func boolPtr(b bool) *bool { return &b }

// TestPipelineRun_Success verifies the happy path: one unified call yields all
// four stage outputs in a single response.
func TestPipelineRun_Success(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:      storage.NewULID(),
		Concept: "test-concept",
		Content: "test content here",
	}

	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("pipeline.Run failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if mock.callCount != 1 {
		t.Fatalf("expected exactly 1 LLM call (unified), got %d", mock.callCount)
	}
	if len(result.Entities) == 0 {
		t.Fatal("expected at least one entity")
	}
	if result.Summary == "" {
		t.Fatal("expected summary")
	}
	if result.MemoryType == "" {
		t.Fatal("expected memory type")
	}
}

// TestPipelineRun_ProviderError covers the case where the unified LLM call
// fails. With nothing carried forward from the engram, the pipeline must
// surface the error.
func TestPipelineRun_ProviderError(t *testing.T) {
	mock := NewMockLLMProvider()
	mock.failCount = 1

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:      storage.NewULID(),
		Concept: "c",
		Content: "x",
	}
	result, err := pipeline.Run(context.Background(), eng)
	if err == nil {
		t.Fatalf("expected error from failed unified call, got result=%+v", result)
	}
	if !strings.Contains(err.Error(), "unified call failed") {
		t.Fatalf("expected unified-call error, got: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result on hard failure with no carry-forward")
	}
}

// TestPipelineRun_ProviderError_CarryForwardWins verifies that when the LLM
// call fails but the engram had inline data that was carried forward, the
// pipeline returns the carry-forward result rather than failing.
func TestPipelineRun_ProviderError_CarryForwardWins(t *testing.T) {
	mock := NewMockLLMProvider()
	mock.failCount = 1

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:         storage.NewULID(),
		Concept:    "c",
		Content:    "x",
		MemoryType: storage.TypeObservation,
		TypeLabel:  "observation",
	}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("expected carry-forward to suppress error, got: %v", err)
	}
	if result == nil || result.MemoryType != "observation" {
		t.Fatalf("expected carry-forward classification, got: %+v", result)
	}
}

// TestPipelineRun_ContextTimeout tests context timeout handling.
func TestPipelineRun_ContextTimeout(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	result, err := pipeline.Run(ctx, eng)
	if err == nil && result == nil {
		t.Fatal("expected error or nil result on timeout")
	}
}

// TestLightMode_OnlyOneLLMCall verifies light mode still results in exactly
// one LLM call. (Same call count as full mode in the unified architecture —
// the difference is the prompt asks only for summary.)
func TestLightMode_OnlyOneLLMCall(t *testing.T) {
	var callCount atomic.Int32
	var capturedSystem string
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		callCount.Add(1)
		capturedSystem = system
		return `{"summary": "Light summary.", "key_points": ["kp1"]}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	pipeline.SetConfig(&config.PluginConfig{EnrichMode: "light"})

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "test", Content: "content"}

	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if callCount.Load() != 1 {
		t.Fatalf("light mode should make exactly 1 LLM call, got %d", callCount.Load())
	}
	// In light mode only the summary stage block should appear in the prompt.
	if !strings.Contains(capturedSystem, "## summary") {
		t.Errorf("light-mode prompt missing summary block: %q", capturedSystem)
	}
	if strings.Contains(capturedSystem, "## entities") {
		t.Errorf("light-mode prompt unexpectedly contains entities block")
	}
	if result.Summary != "Light summary." {
		t.Fatalf("expected light summary, got %q", result.Summary)
	}
	if len(result.Entities) != 0 {
		t.Fatalf("light mode should produce no entities, got %d", len(result.Entities))
	}
}

// TestDisableEntitiesStage drops entities (and relationships) from the prompt
// and from the result when the entities stage is disabled.
func TestDisableEntitiesStage(t *testing.T) {
	var capturedSystem string
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		capturedSystem = system
		// Mock returns entities anyway — the parser must drop them.
		return `{
			"entities": [{"name": "X", "type": "tool", "confidence": 0.9}],
			"summary": "sum", "key_points": ["kp"],
			"memory_type": "fact", "category": "c", "subcategory": "s", "tags": []
		}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	pipeline.SetConfig(&config.PluginConfig{EnrichEntities: boolPtr(false)})

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if strings.Contains(capturedSystem, "## entities") {
		t.Error("entities block must not appear when stage is disabled")
	}
	if strings.Contains(capturedSystem, "## relationships") {
		t.Error("relationships block must not appear when entities stage is disabled")
	}
	if len(result.Entities) != 0 {
		t.Fatalf("expected 0 entities in result, got %d", len(result.Entities))
	}
	if result.Summary != "sum" {
		t.Fatalf("summary should still appear, got %q", result.Summary)
	}
}

// TestDisableClassificationStage drops classification fields from the result
// when the classification stage is disabled.
func TestDisableClassificationStage(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	pipeline.SetConfig(&config.PluginConfig{EnrichClassification: boolPtr(false)})

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if result.MemoryType != "" {
		t.Fatalf("classification should be empty when disabled, got %q", result.MemoryType)
	}
}

// TestDisableSummaryStage drops summary fields from the result.
func TestDisableSummaryStage(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	pipeline.SetConfig(&config.PluginConfig{EnrichSummary: boolPtr(false)})

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if result.Summary != "" {
		t.Fatalf("summary should be empty when disabled, got %q", result.Summary)
	}
	if result.MemoryType == "" {
		t.Fatal("classification should still run when only summary is disabled")
	}
}

// TestDisableRelationshipsStage_EntitiesStillExtracted: disabling relationships
// doesn't affect entity extraction.
func TestDisableRelationshipsStage_EntitiesStillExtracted(t *testing.T) {
	var capturedSystem string
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		capturedSystem = system
		return `{
			"entities": [{"name": "Go", "type": "language", "confidence": 0.95}],
			"summary": "s", "key_points": ["k"],
			"memory_type": "fact", "category": "c", "subcategory": "s", "tags": []
		}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)
	pipeline.SetConfig(&config.PluginConfig{EnrichRelationships: boolPtr(false)})

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if strings.Contains(capturedSystem, "## relationships") {
		t.Error("relationships block must not appear when stage is disabled")
	}
	if !strings.Contains(capturedSystem, "## entities") {
		t.Error("entities block must still appear")
	}
	if len(result.Entities) != 1 {
		t.Fatalf("entities should still be extracted, got %d", len(result.Entities))
	}
	if len(result.Relationships) != 0 {
		t.Fatalf("relationships should be empty when disabled, got %d", len(result.Relationships))
	}
}

// TestClassificationExpandedEnum verifies all memory types map correctly.
func TestClassificationExpandedEnum(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType storage.MemoryType
	}{
		{"fact", "fact", storage.TypeFact},
		{"decision", "decision", storage.TypeDecision},
		{"observation", "observation", storage.TypeObservation},
		{"preference", "preference", storage.TypePreference},
		{"issue", "issue", storage.TypeIssue},
		{"bugfix alias", "bugfix", storage.TypeIssue},
		{"bug_report alias", "bug_report", storage.TypeIssue},
		{"task", "task", storage.TypeTask},
		{"procedure", "procedure", storage.TypeProcedure},
		{"event", "event", storage.TypeEvent},
		{"experience alias", "experience", storage.TypeEvent},
		{"goal", "goal", storage.TypeGoal},
		{"constraint", "constraint", storage.TypeConstraint},
		{"identity", "identity", storage.TypeIdentity},
		{"reference", "reference", storage.TypeReference},
		{"unknown defaults to fact", "unknown_thing", storage.TypeFact},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mt, _ := resolveClassification(tt.input, "")
			if mt != tt.wantType {
				t.Errorf("resolveClassification(%q) = %d, want %d", tt.input, mt, tt.wantType)
			}
		})
	}
}

// TestClassificationWithTypeLabel verifies type_label is preferred over memory_type.
func TestClassificationWithTypeLabel(t *testing.T) {
	mt, display := resolveClassification("decision", "architectural_decision")
	if mt != storage.TypeDecision {
		t.Errorf("expected TypeDecision, got %d", mt)
	}
	if display != "architectural_decision" {
		t.Errorf("expected display 'architectural_decision', got %q", display)
	}
}

// TestSkipIfPresent_Summary verifies summary is skipped when engram has one;
// the carry-forward path populates result.Summary with the engram's existing
// value. The remaining stages still fire in a single unified call.
func TestSkipIfPresent_Summary(t *testing.T) {
	var capturedSystem string
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		capturedSystem = system
		return `{
			"entities": [{"name": "X", "type": "tool", "confidence": 0.9}],
			"memory_type": "fact", "category": "c", "subcategory": "s", "tags": []
		}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:      storage.NewULID(),
		Concept: "c",
		Content: "x",
		Summary: "existing summary",
	}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if strings.Contains(capturedSystem, "## summary") {
		t.Error("summary block must not appear when summary already exists on engram")
	}
	if result.Summary != "existing summary" {
		t.Fatalf("expected carried-forward summary, got %q", result.Summary)
	}
}

// TestSkipIfPresent_Classification: with engram MemoryType set, the
// classification stage is excluded from the prompt and carry-forward
// populates the result.
func TestSkipIfPresent_Classification(t *testing.T) {
	var capturedSystem string
	mock := NewMockLLMProvider()
	mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
		capturedSystem = system
		return `{
			"entities": [{"name": "X", "type": "tool", "confidence": 0.9}],
			"summary": "s", "key_points": ["k"]
		}`, nil
	}

	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:         storage.NewULID(),
		Concept:    "c",
		Content:    "x",
		MemoryType: storage.TypeDecision,
	}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if strings.Contains(capturedSystem, "## classification") {
		t.Error("classification block must not appear when MemoryType already set")
	}
	if result.MemoryType != "decision" {
		t.Fatalf("expected carried-forward MemoryType, got %q", result.MemoryType)
	}
}

// TestSkipIfPresent_Entities documents that entity extraction is skipped only
// when both KeyPoints AND Summary are present (caller provided full
// enrichment). Only KeyPoints — no skip.
func TestSkipIfPresent_Entities(t *testing.T) {
	limiter := NewTokenBucketLimiter(100.0, 100.0)

	t.Run("KeyPointsOnly_MustNotSkip", func(t *testing.T) {
		var capturedSystem string
		mock := NewMockLLMProvider()
		mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
			capturedSystem = system
			return `{
				"entities": [{"name": "Z", "type": "tool", "confidence": 0.8}],
				"summary": "s", "key_points": ["k"],
				"memory_type": "fact", "category": "c", "subcategory": "s", "tags": []
			}`, nil
		}
		pipeline := NewPipeline(mock, limiter)
		eng := &storage.Engram{
			ID:        storage.NewULID(),
			Concept:   "c",
			Content:   "x",
			KeyPoints: []string{"existing key point"},
		}
		_, err := pipeline.Run(context.Background(), eng)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if !strings.Contains(capturedSystem, "## entities") {
			t.Fatal("entities block must appear when only KeyPoints are set (no Summary)")
		}
	})

	t.Run("KeyPointsAndSummary_MaySkip", func(t *testing.T) {
		var capturedSystem string
		mock := NewMockLLMProvider()
		mock.customComplete = func(_ context.Context, system, _ string) (string, error) {
			capturedSystem = system
			return `{"memory_type": "fact", "category": "c", "subcategory": "s", "tags": []}`, nil
		}
		pipeline := NewPipeline(mock, limiter)
		eng := &storage.Engram{
			ID:        storage.NewULID(),
			Concept:   "c",
			Content:   "x",
			KeyPoints: []string{"existing key point"},
			Summary:   "existing summary",
		}
		_, err := pipeline.Run(context.Background(), eng)
		if err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if strings.Contains(capturedSystem, "## entities") {
			t.Fatal("entities block must not appear when both KeyPoints and Summary set")
		}
	})
}

// TestPipelineRun_AllStagesSkipped_CarriesInlineData verifies that when all
// stages are skipped because the engram already has inline data, no LLM call
// is made and the inline data is carried forward into the result.
func TestPipelineRun_AllStagesSkipped_CarriesInlineData(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:         storage.NewULID(),
		Concept:    "pre-enriched",
		Content:    "already has everything",
		Summary:    "existing summary",
		KeyPoints:  []string{"kp1"},
		MemoryType: storage.TypeDecision,
	}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result carrying inline data")
	}
	if mock.callCount != 0 {
		t.Fatalf("expected 0 LLM calls, got %d", mock.callCount)
	}
	if result.Summary != "existing summary" {
		t.Fatalf("expected carried-forward summary, got %q", result.Summary)
	}
	if len(result.KeyPoints) != 1 || result.KeyPoints[0] != "kp1" {
		t.Fatalf("expected carried-forward key points, got %v", result.KeyPoints)
	}
	if result.MemoryType != "decision" {
		t.Fatalf("expected carried-forward memory type 'decision', got %q", result.MemoryType)
	}
}

// TestSkipClassification_CarriesInlineDataForDigestFlag verifies that when an
// engram has inline classification (the digest-flag bug scenario), the
// pipeline carries it forward into the result so UpdateDigest sets
// DigestClassified.
func TestSkipClassification_CarriesInlineDataForDigestFlag(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:         storage.NewULID(),
		Concept:    "inline-classified",
		Content:    "content with inline classification",
		MemoryType: storage.TypeObservation,
		TypeLabel:  "observation",
	}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if result.MemoryType != "observation" {
		t.Fatalf("expected carried-forward MemoryType 'observation', got %q", result.MemoryType)
	}
	if result.TypeLabel != "observation" {
		t.Fatalf("expected carried-forward TypeLabel 'observation', got %q", result.TypeLabel)
	}
}

// TestSkipSummary_CarriesInlineDataForDigestFlag mirrors the classification
// case for the Summary stage.
func TestSkipSummary_CarriesInlineDataForDigestFlag(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	eng := &storage.Engram{
		ID:        storage.NewULID(),
		Concept:   "inline-summarized",
		Content:   "content with inline summary",
		Summary:   "pre-set summary",
		KeyPoints: []string{"pre-set kp"},
	}
	result, err := pipeline.Run(context.Background(), eng)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if result.Summary != "pre-set summary" {
		t.Fatalf("expected carried-forward Summary, got %q", result.Summary)
	}
	if len(result.KeyPoints) != 1 || result.KeyPoints[0] != "pre-set kp" {
		t.Fatalf("expected carried-forward KeyPoints, got %v", result.KeyPoints)
	}
}

// TestStagesToRun_TableDriven exercises the stage-selection helper directly so
// regressions in the dynamic-prompt logic surface fast.
func TestStagesToRun_TableDriven(t *testing.T) {
	limiter := NewTokenBucketLimiter(100.0, 100.0)

	tests := []struct {
		name string
		cfg  *config.PluginConfig
		eng  *storage.Engram
		want []string
	}{
		{
			name: "default config, fresh engram",
			cfg:  nil,
			eng:  &storage.Engram{},
			want: []string{"entities", "relationships", "classification", "summary"},
		},
		{
			name: "light mode",
			cfg:  &config.PluginConfig{EnrichMode: "light"},
			eng:  &storage.Engram{},
			want: []string{"summary"},
		},
		{
			name: "summary disabled",
			cfg:  &config.PluginConfig{EnrichSummary: boolPtr(false)},
			eng:  &storage.Engram{},
			want: []string{"entities", "relationships", "classification"},
		},
		{
			name: "entities disabled drops relationships too",
			cfg:  &config.PluginConfig{EnrichEntities: boolPtr(false)},
			eng:  &storage.Engram{},
			want: []string{"classification", "summary"},
		},
		{
			name: "summary already inline, classification already inline",
			cfg:  nil,
			eng:  &storage.Engram{Summary: "s", MemoryType: storage.TypeDecision},
			want: []string{"entities", "relationships"},
		},
		{
			name: "fully pre-enriched -> no stages",
			cfg:  nil,
			eng: &storage.Engram{
				Summary:    "s",
				KeyPoints:  []string{"k"},
				MemoryType: storage.TypeDecision,
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPipeline(NewMockLLMProvider(), limiter)
			p.SetConfig(tt.cfg)
			got := p.stagesToRun(tt.eng)
			if !stringSliceEqual(got, tt.want) {
				t.Fatalf("stagesToRun: got %v, want %v", got, tt.want)
			}
		})
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBuildUnifiedPrompt verifies the prompt builder is byte-stable for a
// given enabled-set and respects canonical stage order.
func TestBuildUnifiedPrompt(t *testing.T) {
	// Same stages, different input order — outputs must match.
	a := BuildUnifiedPrompt([]string{"summary", "entities", "classification", "relationships"})
	b := BuildUnifiedPrompt([]string{"entities", "relationships", "classification", "summary"})
	if a != b {
		t.Fatalf("BuildUnifiedPrompt must be order-independent; got mismatched outputs")
	}

	// Empty input -> empty output.
	if BuildUnifiedPrompt(nil) != "" {
		t.Fatal("empty input should yield empty prompt")
	}
	if BuildUnifiedPrompt([]string{}) != "" {
		t.Fatal("empty slice should yield empty prompt")
	}

	// Only enabled stages appear in the prompt.
	only := BuildUnifiedPrompt([]string{"summary"})
	if !strings.Contains(only, "## summary") {
		t.Fatal("expected summary block")
	}
	if strings.Contains(only, "## entities") || strings.Contains(only, "## relationships") || strings.Contains(only, "## classification") {
		t.Fatal("disabled stage blocks must not appear")
	}

	// Unknown stages are filtered out.
	if BuildUnifiedPrompt([]string{"unknown"}) != "" {
		t.Fatal("unknown-only stages should yield empty prompt")
	}
}
