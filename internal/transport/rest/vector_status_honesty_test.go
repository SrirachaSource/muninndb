package rest

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// Muninn caught it 2026-07-19: semantic_searchable was presence-derived
// (graph_in_memory && !tombstoned) and read TRUE on every in-link-starved
// orphan -- present in the graph, invisible to Search. Only the self-probe
// tests reachability, so the field must be probe-gated: absent without a
// probe (unknown), and the probe's verdict with one. Presence lives in the
// honestly-named graph_present.
func TestVectorStatus_SemanticSearchableIsProbeGated(t *testing.T) {
	s := newTestServer(t, nil)

	// Without a probe: presence reported, searchability UNKNOWN (key absent).
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET",
		"/api/admin/vaults/v/engrams/01HNKZ5F00000000000000000A/vector-status", nil)
	s.mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("no-probe: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["graph_present"] != true {
		t.Errorf("graph_present = %v, want true (node IS in the graph)", raw["graph_present"])
	}
	if _, present := raw["semantic_searchable"]; present {
		t.Errorf("semantic_searchable emitted WITHOUT a probe (value %v) -- "+
			"presence must never claim searchability", raw["semantic_searchable"])
	}

	// With a probe that misses (the blind orphan): searchability is FALSE.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET",
		"/api/admin/vaults/v/engrams/01HNKZ5F00000000000000000A/vector-status?probe=1&k=100", nil)
	s.mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("probe: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	raw = map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["graph_present"] != true {
		t.Errorf("graph_present = %v, want true", raw["graph_present"])
	}
	v, present := raw["semantic_searchable"]
	if !present || v != false {
		t.Errorf("semantic_searchable = %v (present=%v), want explicit false: "+
			"in the graph but the probe cannot find it", v, present)
	}
}
