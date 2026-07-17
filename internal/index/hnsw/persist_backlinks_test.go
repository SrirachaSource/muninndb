package hnsw

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

// TestInsertPersistsNeighborBacklinks pins the invariant that the graph on DISK
// is the graph in MEMORY.
//
// WHY: Insert wires bidirectional links in memory -- the new node's list AND an
// appended back-link on each chosen neighbor -- but then persists ONLY the new
// node (`go idx.persistNode(id, node)`). The neighbors' mutated lists never
// reach Pebble, so the persisted graph holds forward edges only (new -> old).
// Measured on the 2026-07-17 floor exports: 955,876 stored layer-0 edges in the
// trading vault, FOUR reciprocal. After every restart, LoadFromPebble rebuilds
// that DAG, greedy search descends from an old high-layer entry point through
// edges that point at even OLDER nodes, and everything inserted after the
// reachable core becomes invisible to semantic recall -- silently, because a
// search over a fragmented graph returns fewer/worse results, not an error.
// That is the mechanism behind "the trading vault has no semantic recall"
// (30,059 clean vectors, 13 disconnected components, largest 38.8%).
//
// This test FAILS on the unfixed code. That failure IS the reproduction.
func TestInsertPersistsNeighborBacklinks(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	ws := [8]byte{9, 9, 9, 9, 9, 9, 9, 9}
	idA := [16]byte{0xA1}
	idB := [16]byte{0xB2}
	vecA := []float32{1, 0, 0, 0}
	vecB := []float32{0.9, 0.1, 0, 0} // close to A so B chooses A as a neighbor

	idx := New(db, ws)
	if err := idx.StoreVector(idA, vecA); err != nil {
		t.Fatalf("store A: %v", err)
	}
	idx.Insert(idA, vecA)
	// Era boundary: wait for A's persist to land BEFORE B arrives. Without this
	// the test can pass by accident -- persistNode runs in a goroutine, so
	// Insert(A)'s write sometimes happens AFTER Insert(B) mutated A in memory,
	// persisting the back-link by scheduling luck. In production the "luck"
	// window closes at every restart; this Close() models that boundary.
	idx.Close()

	if err := idx.StoreVector(idB, vecB); err != nil {
		t.Fatalf("store B: %v", err)
	}
	idx.Insert(idB, vecB)
	// In memory, A's layer-0 list now contains B (the back-link Insert added).
	idx.Close() // waits for all in-flight persists

	// Reload from Pebble into a FRESH index: disk is now the only truth.
	fresh := New(db, ws)
	if err := fresh.LoadFromPebble(); err != nil {
		t.Fatalf("LoadFromPebble: %v", err)
	}

	a := fresh.nodes[idA]
	if a == nil {
		t.Fatalf("node A missing after reload")
	}
	found := false
	a.mu.RLock()
	for _, layer := range a.layers {
		for _, nb := range layer {
			if nb == idB {
				found = true
			}
		}
	}
	a.mu.RUnlock()
	if !found {
		t.Errorf("A's persisted neighbor lists do not contain B: the in-memory "+
			"back-link was never written to Pebble. On-disk graph = forward-only "+
			"DAG; every restart strands post-core inserts (A layers: %d)", len(a.layers))
	}

	// The consequence search-side: B must be findable from the reloaded graph.
	fresh2 := New(db, ws)
	if err := fresh2.LoadFromPebble(); err != nil {
		t.Fatalf("LoadFromPebble: %v", err)
	}
	res, err := fresh2.Search(context.Background(), vecB, 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	gotB := false
	for _, r := range res {
		if r.ID == idB {
			gotB = true
		}
	}
	if !gotB {
		t.Errorf("reloaded index cannot find B by B's own vector -- search is "+
			"confined to the pre-existing core (got %d results)", len(res))
	}
}
