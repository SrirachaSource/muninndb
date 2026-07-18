package engine

import (
	"context"
	"errors"
	"testing"
)

// TestEngineGetContradictions_Empty verifies that querying contradictions on a
// fresh vault returns an empty slice without error.
func TestEngineGetContradictions_Empty(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	pairs, err := eng.GetContradictions(ctx, "empty-vault")
	if err != nil {
		t.Fatalf("GetContradictions on empty vault returned error: %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("expected 0 contradiction pairs on fresh vault, got %d", len(pairs))
	}
}

// TestEngineExplain_UnknownID verifies that Explain fails LOUD on lookup
// failures instead of returning an all-zeros result: a malformed ID errors at
// parse, and a well-formed ID that was never written returns ErrEngramNotFound.
// (The old contract — silent zeros for any miss — made a lookup failure
// indistinguishable from a genuine zero score.)
func TestEngineExplain_UnknownID(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Malformed (24 chars — a ULID is 26): must error at parse, like Read.
	if _, err := eng.Explain(ctx, "test-vault", "01HNKZ5F0000000000000000", []string{"anything"}, nil); err == nil {
		t.Fatal("Explain with malformed ID returned nil error, expected parse error")
	}

	// Well-formed but never written: must return ErrEngramNotFound.
	_, err := eng.Explain(ctx, "test-vault", "01HNKZ5F00000000000000000A", []string{"anything"}, nil)
	if !errors.Is(err, ErrEngramNotFound) {
		t.Fatalf("Explain with unknown ID: got err=%v, want ErrEngramNotFound", err)
	}
}

// TestQueryMethodsCompilable is a compile-time proof that all four query
// methods are callable on *Engine via the query.go file. If any method
// were missing or had the wrong signature, this file would fail to compile.
func TestQueryMethodsCompilable(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// GetContradictions
	_, err := eng.GetContradictions(ctx, "vault")
	_ = err

	// GetAssociations
	_, err = eng.GetAssociations(ctx, "vault", "01HNKZ5F0000000000000000", 10)
	_ = err

	// Traverse
	_, _, err = eng.Traverse(ctx, "vault", "01HNKZ5F0000000000000000", 2, 10, false)
	_ = err

	// Explain
	_, err = eng.Explain(ctx, "vault", "01HNKZ5F0000000000000000", []string{"query"}, nil)
	_ = err
}
