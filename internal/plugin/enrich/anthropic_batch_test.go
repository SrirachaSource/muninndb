package enrich

import (
	"context"
	"errors"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// mockBatchProvider implements LLMProvider + BatchLLMProvider with no network.
// It records which items were submitted and returns a canned unified JSON for
// each (or a per-request error for ids in failIDs).
type mockBatchProvider struct {
	submitted []BatchItem
	failIDs   map[string]bool
	respJSON  string
}

func (m *mockBatchProvider) Name() string                                          { return "mock-batch" }
func (m *mockBatchProvider) Init(context.Context, LLMProviderConfig) error          { return nil }
func (m *mockBatchProvider) Complete(context.Context, string, string) (string, error) { return "", nil }
func (m *mockBatchProvider) Close() error                                           { return nil }

func (m *mockBatchProvider) SubmitBatch(_ context.Context, items []BatchItem) (string, error) {
	m.submitted = append(m.submitted, items...)
	return "msgbatch_test", nil
}

func (m *mockBatchProvider) PollBatch(_ context.Context, id string) (BatchStatus, error) {
	// End immediately so the poll loop returns without sleeping.
	return BatchStatus{ID: id, Status: "ended", Ended: true, ResultsURL: "https://example/results"}, nil
}

func (m *mockBatchProvider) FetchBatchResults(context.Context, string) (map[string]BatchResult, error) {
	out := make(map[string]BatchResult, len(m.submitted))
	for _, it := range m.submitted {
		if m.failIDs[it.CustomID] {
			out[it.CustomID] = BatchResult{Err: "errored"}
		} else {
			out[it.CustomID] = BatchResult{Text: m.respJSON}
		}
	}
	return out, nil
}

const testUnifiedJSON = `{
	"entities": [{"name": "PostgreSQL", "type": "database", "confidence": 0.95}],
	"relationships": [],
	"memory_type": "decision",
	"type_label": "",
	"category": "infrastructure",
	"subcategory": "databases",
	"tags": ["db"],
	"summary": "Batch summary.",
	"key_points": ["kp1", "kp2"]
}`

// TestEnrichBatch_SubmitsOnlyEngramsNeedingWork is the core correctness guard:
// a fully-inline engram (nothing to enrich) must NOT be submitted to the batch
// and must NOT be reported as an error — it comes back with its own data. This
// is exactly the sweep-vs-pipeline skip subtlety the wiring must respect.
func TestEnrichBatch_SubmitsOnlyEngramsNeedingWork(t *testing.T) {
	mock := &mockBatchProvider{respJSON: testUnifiedJSON}
	p := NewPipeline(mock, NewTokenBucketLimiter(100, 100))

	needs := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "needs enrichment"}
	// Summary + KeyPoints + non-Fact MemoryType => every stage is inline =>
	// stagesToRun is empty => must be skipped (not submitted).
	skip := &storage.Engram{
		ID: storage.NewULID(), Concept: "c2", Content: "x",
		Summary: "already done", KeyPoints: []string{"k"}, MemoryType: storage.TypeDecision,
	}

	results, errs, err := p.EnrichBatch(context.Background(), []*storage.Engram{needs, skip})
	if err != nil {
		t.Fatalf("EnrichBatch returned a whole-batch error: %v", err)
	}

	// The skip engram must NOT have been submitted.
	for _, it := range mock.submitted {
		if it.CustomID == skip.ID.String() {
			t.Fatal("fully-inline engram was wrongly submitted to the batch (wasted spend)")
		}
	}
	if len(mock.submitted) != 1 || mock.submitted[0].CustomID != needs.ID.String() {
		t.Fatalf("expected only the needs-enrichment engram submitted, got %d items", len(mock.submitted))
	}

	// The needs engram came back parsed from the batch.
	rNeeds := results[needs.ID.String()]
	if rNeeds == nil {
		t.Fatal("missing result for the needs-enrichment engram")
	}
	if rNeeds.Summary != "Batch summary." {
		t.Fatalf("batch summary not parsed onto needs engram: %q", rNeeds.Summary)
	}
	if len(rNeeds.Entities) != 1 {
		t.Fatalf("batch entities not parsed onto needs engram: %v", rNeeds.Entities)
	}

	// The skip engram must come back with its carried-forward data, NOT an error.
	if _, isErr := errs[skip.ID.String()]; isErr {
		t.Fatal("fully-inline engram was wrongly reported as an error (would mis-mark it failed)")
	}
	rSkip := results[skip.ID.String()]
	if rSkip == nil || rSkip.Summary != "already done" {
		t.Fatalf("skip engram should return its inline data, got %+v", rSkip)
	}
}

// TestEnrichBatch_PerRequestErrorSurfaced verifies a failed batch request lands
// in errs (so the caller can mark it failed / retry it) and not in results.
func TestEnrichBatch_PerRequestErrorSurfaced(t *testing.T) {
	needs := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	mock := &mockBatchProvider{
		respJSON: testUnifiedJSON,
		failIDs:  map[string]bool{needs.ID.String(): true},
	}
	p := NewPipeline(mock, NewTokenBucketLimiter(100, 100))

	results, errs, err := p.EnrichBatch(context.Background(), []*storage.Engram{needs})
	if err != nil {
		t.Fatalf("unexpected whole-batch error: %v", err)
	}
	if _, ok := results[needs.ID.String()]; ok {
		t.Fatal("an errored request must not produce a result")
	}
	if errs[needs.ID.String()] == nil {
		t.Fatal("expected the per-request error surfaced in errs")
	}
}

// TestEnrichBatch_UnsupportedProvider verifies a non-batch provider yields
// ErrBatchUnsupported so the sweep falls back to the synchronous path.
func TestEnrichBatch_UnsupportedProvider(t *testing.T) {
	p := NewPipeline(NewMockLLMProvider(), NewTokenBucketLimiter(100, 100))
	_, _, err := p.EnrichBatch(context.Background(), []*storage.Engram{{ID: storage.NewULID()}})
	if !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("expected ErrBatchUnsupported for a non-batch provider, got %v", err)
	}
}
