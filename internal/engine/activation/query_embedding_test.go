package activation

import (
	"context"
	"strings"
	"testing"
)

// recordingEmbedder captures exactly what the query path hands to Embed, and
// models the REAL batch contract: N texts -> N vectors flattened (len == N*dim).
// That contract is deliberate -- retroactive.go and brief_scorer_adapter.go both
// depend on it (the latter derives dim as len(flat)/len(texts)) -- so it must be
// preserved here, not "fixed".
type recordingEmbedder struct {
	gotTexts []string
}

const testEmbedDim = 384

func (e *recordingEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	e.gotTexts = append([]string(nil), texts...)
	return make([]float32, len(texts)*testEmbedDim), nil
}

func (e *recordingEmbedder) Tokenize(text string) []string { return strings.Fields(text) }

// stubHNSWForEmbed exists only so phase1 takes the embedding branch (which is
// gated on e.hnsw != nil). It is never queried by phase1.
type stubHNSWForEmbed struct{}

func (s *stubHNSWForEmbed) Search(_ context.Context, _ [8]byte, _ []float32, _ int) ([]ScoredID, error) {
	return nil, nil
}

// TestPhase1EmbedsQueryAsSingleVector is the regression lock for the 2026-07-16
// false-zero bug: phase1 passed req.Context (a []string) straight to the BATCH
// embedder, so an N-phrase context produced an N*dim vector. The HNSW index
// expects dim, so semantic similarity came back exactly 0 -- vector_score was
// dropped via omitempty and rank fell to FTS+recency alone. Confident garbage,
// and the more phrases a desk searched with, the worse it got.
//
// THIS TEST FAILS ON THE PRE-FIX CODE (it observes 3 texts / 1152 floats) and
// passes on the fix. A fixture that cannot fail proves nothing.
func TestPhase1EmbedsQueryAsSingleVector(t *testing.T) {
	multiPhrase := []string{"MuninnDB backup", "fly ssh to Google Drive", "backup.yml never ran"}

	emb := &recordingEmbedder{}
	e := New(nil, nil, &stubHNSWForEmbed{}, emb)

	res, err := e.phase1(context.Background(), &ActivateRequest{Context: multiPhrase})
	if err != nil {
		t.Fatalf("phase1: %v", err)
	}

	// 1. The query is embedded as exactly ONE text, not N.
	if len(emb.gotTexts) != 1 {
		t.Fatalf("query embedded as %d texts %q; want exactly 1 -- a batch embedder given N "+
			"texts returns N vectors flattened, which is not a query vector",
			len(emb.gotTexts), emb.gotTexts)
	}

	// 2. That one text is the SAME joined string used for Tokenize and the FTS
	//    query -- the vector query and the text query must not diverge.
	if emb.gotTexts[0] != res.queryStr {
		t.Errorf("embedded %q but queryStr is %q; vector query and text query diverged",
			emb.gotTexts[0], res.queryStr)
	}

	// 3. The resulting embedding is dim-sized, not N*dim -- this is the value
	//    handed to the HNSW index.
	if len(res.embedding) != testEmbedDim {
		t.Errorf("embedding len = %d; want %d (got %dx dim -- a concatenation of "+
			"per-phrase vectors, malformed for a dim-sized index)",
			len(res.embedding), testEmbedDim, len(res.embedding)/testEmbedDim)
	}
}

// TestPhase1SinglePhraseUnchanged pins that the fix cannot regress the healthy
// path: a one-phrase context behaved correctly before and must still do so.
func TestPhase1SinglePhraseUnchanged(t *testing.T) {
	emb := &recordingEmbedder{}
	e := New(nil, nil, &stubHNSWForEmbed{}, emb)

	res, err := e.phase1(context.Background(), &ActivateRequest{Context: []string{"kinds of nothing"}})
	if err != nil {
		t.Fatalf("phase1: %v", err)
	}
	if len(emb.gotTexts) != 1 || emb.gotTexts[0] != "kinds of nothing" {
		t.Errorf("single-phrase embed got %q; want exactly [\"kinds of nothing\"]", emb.gotTexts)
	}
	if len(res.embedding) != testEmbedDim {
		t.Errorf("embedding len = %d; want %d", len(res.embedding), testEmbedDim)
	}
}
