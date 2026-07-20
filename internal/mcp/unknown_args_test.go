package mcp

// Regression tests for the silent field-drop defect (2026-07-19): a caller
// passed "content" to muninn_evolve instead of "new_content"; the unknown key
// was ignored without error and a placeholder string was written as the
// engram's entire content. Write-path mutators must reject unknown arguments
// loudly.

import (
	"strings"
	"testing"
)

func TestHandleEvolve_UnknownArgRejected(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_evolve","arguments":{"vault":"default","id":"old-id","new_content":"placeholder","reason":"update","content":"the real payload the caller meant to write"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("expected -32602 for unknown arg, got %v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "content") || !strings.Contains(resp.Error.Message, "unknown") {
		t.Errorf("error should name the unknown arg, got: %s", resp.Error.Message)
	}
}

func TestHandleEvolve_ConceptStillAccepted(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_evolve","arguments":{"vault":"default","id":"old-id","new_content":"updated","reason":"stale","concept":"fresh title"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("concept is a schema arg and must pass the guard: %v", resp.Error)
	}
}

func TestHandleRemember_UnknownArgRejected(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"a fact","text":"typo field"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("expected -32602 for unknown arg, got %v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "text") {
		t.Errorf("error should name the unknown arg, got: %s", resp.Error.Message)
	}
}

func TestHandleRemember_FullSchemaStillAccepted(t *testing.T) {
	// Every schema-documented argument must pass the guard — in particular
	// "relationships" and "entity_relationships", which live callers set.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{
		"vault":"default","content":"a fact","concept":"title","tags":["t1"],
		"confidence":0.9,"type":"fact","type_label":"note","summary":"s",
		"entities":[{"name":"KLAC","type":"company"}],
		"relationships":[{"target_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","relation":"supports"}],
		"entity_relationships":[{"from_entity":"KLAC","to_entity":"chip-tools","rel_type":"belongs_to"}],
		"op_id":"op-guard-test-1"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("all schema args must pass the guard: %v", resp.Error)
	}
}
