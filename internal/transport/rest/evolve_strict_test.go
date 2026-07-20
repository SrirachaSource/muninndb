package rest

// Regression tests for the silent field-drop defect (2026-07-19): the evolve
// body decoder ignored unknown fields, so a caller passing "content" instead
// of "new_content" lost the real payload without an error. The decoder is now
// strict, and REST evolve carries the concept param (parity with MCP).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEvolveEndpoint_UnknownFieldRejected(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"new_content":"placeholder","reason":"update","content":"the real payload"}`
	req := httptest.NewRequest("POST", "/api/engrams/"+testEngramID+"/evolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown body field, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "content") {
		t.Errorf("error should name the unknown field, got: %s", w.Body.String())
	}
}

func TestEvolveEndpoint_ConceptPassthrough(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"new_content":"updated","reason":"stale","concept":"fresh title"}`
	req := httptest.NewRequest("POST", "/api/engrams/"+testEngramID+"/evolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if engine.lastEvolveConcept != "fresh title" {
		t.Errorf("concept not passed to engine: want %q, got %q", "fresh title", engine.lastEvolveConcept)
	}
}
