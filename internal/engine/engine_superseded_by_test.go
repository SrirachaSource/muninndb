package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Evolve forks: it mints a NEW ULID for the successor and soft-deletes the
// predecessor. The predecessor's id keeps resolving, and Read keeps serving its
// (now superseded) content with HTTP-level success and no indication that a
// corrected version exists. Every id ever written into a note, a doc or another
// system is therefore a delayed-fuse stale pointer: it works, until someone
// evolves the memory, and from then on it silently returns the version that was
// corrected.
//
// The successor is discoverable -- it carries a RelSupersedes edge pointing BACK
// at the predecessor, which the reverse index can answer -- but the read path
// never looks. These tests pin that a read of a superseded engram names its
// successor.

// TestRead_SupersededEngram_NamesItsSuccessor: the core contract. Reading the
// old id must reveal that it was superseded, and by whom.
func TestRead_SupersededEngram_NamesItsSuccessor(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	orig, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Claim", Content: "the reactor ignites in 2027",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	newID, err := eng.Evolve(ctx, "test", orig.ID, "the reactor ignites in 2031", "corrected", nil)
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}

	// Read the id a caller would have written down BEFORE the evolve.
	resp, err := eng.Read(ctx, &mbp.ReadRequest{ID: orig.ID, Vault: "test"})
	if err != nil {
		t.Fatalf("Read old id: %v", err)
	}

	if resp.SupersededBy == "" {
		t.Errorf("read of a superseded engram did not name its successor: "+
			"SupersededBy is empty, so the caller is handed %q with no way to learn "+
			"a corrected version exists", resp.Content)
	}
	if resp.SupersededBy != newID.String() {
		t.Errorf("SupersededBy = %q, want %q (the successor Evolve returned)",
			resp.SupersededBy, newID.String())
	}
}

// TestRead_LiveEngram_HasNoSupersededBy: the field must stay empty for a normal
// engram. A successor pointer on a live memory would be worse than none -- it
// would send readers somewhere else for no reason.
func TestRead_LiveEngram_HasNoSupersededBy(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Live", Content: "never evolved",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	read, err := eng.Read(ctx, &mbp.ReadRequest{ID: resp.ID, Vault: "test"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read.SupersededBy != "" {
		t.Errorf("live engram reports SupersededBy = %q, want empty", read.SupersededBy)
	}
}

// TestRead_EvolveChain_NamesTheImmediateSuccessor: evolve twice. Each corpse
// must point at the version that directly replaced it, so a reader can walk the
// chain forward one hop at a time and land on the live memory.
func TestRead_EvolveChain_NamesTheImmediateSuccessor(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	v1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Chain", Content: "version one",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	v2, err := eng.Evolve(ctx, "test", v1.ID, "version two", "r1", nil)
	if err != nil {
		t.Fatalf("Evolve 1: %v", err)
	}
	v3, err := eng.Evolve(ctx, "test", v2.String(), "version three", "r2", nil)
	if err != nil {
		t.Fatalf("Evolve 2: %v", err)
	}

	// v1 -> v2
	r1, err := eng.Read(ctx, &mbp.ReadRequest{ID: v1.ID, Vault: "test"})
	if err != nil {
		t.Fatalf("Read v1: %v", err)
	}
	if r1.SupersededBy != v2.String() {
		t.Errorf("v1.SupersededBy = %q, want v2 %q", r1.SupersededBy, v2.String())
	}

	// v2 -> v3
	r2, err := eng.Read(ctx, &mbp.ReadRequest{ID: v2.String(), Vault: "test"})
	if err != nil {
		t.Fatalf("Read v2: %v", err)
	}
	if r2.SupersededBy != v3.String() {
		t.Errorf("v2.SupersededBy = %q, want v3 %q", r2.SupersededBy, v3.String())
	}

	// v3 is live and terminates the chain.
	r3, err := eng.Read(ctx, &mbp.ReadRequest{ID: v3.String(), Vault: "test"})
	if err != nil {
		t.Fatalf("Read v3: %v", err)
	}
	if r3.SupersededBy != "" {
		t.Errorf("v3 is live but reports SupersededBy = %q", r3.SupersededBy)
	}
}
