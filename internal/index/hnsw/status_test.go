package hnsw_test

import (
	"context"
	"math/rand"
	"testing"

	"github.com/scrypster/muninndb/internal/index/hnsw"
)

// TestNodeStatus_PresentAbsentTombstoned covers the three membership states.
func TestNodeStatus_PresentAbsentTombstoned(t *testing.T) {
	db := newTestDB(t)
	idx := hnsw.New(db, testWS())
	t.Cleanup(idx.Close) // drain persistNode goroutines before db.Close() fires
	rng := rand.New(rand.NewSource(7))

	inserted := newID(rng)
	absent := newID(rng)
	dead := newID(rng)
	idx.Insert(inserted, randomUnitVector(rng, 8))
	idx.Insert(dead, randomUnitVector(rng, 8))
	idx.Tombstone(dead)

	present, edges, tomb := idx.NodeStatus(inserted)
	if !present || tomb {
		t.Fatalf("inserted node: present=%v tombstoned=%v, want true/false", present, tomb)
	}
	if len(edges) == 0 {
		t.Fatalf("inserted node: expected at least one layer of edge counts, got none")
	}

	present, edges, tomb = idx.NodeStatus(absent)
	if present || tomb || edges != nil {
		t.Fatalf("absent node: present=%v edges=%v tombstoned=%v, want false/nil/false", present, edges, tomb)
	}

	present, _, tomb = idx.NodeStatus(dead)
	if !present || !tomb {
		t.Fatalf("tombstoned node: present=%v tombstoned=%v, want true/true", present, tomb)
	}
}

// TestStoredVectorDim distinguishes a persisted 0xFF slot from no slot at all.
func TestStoredVectorDim(t *testing.T) {
	db := newTestDB(t)
	idx := hnsw.New(db, testWS())
	t.Cleanup(idx.Close) // drain persistNode goroutines before db.Close() fires
	rng := rand.New(rand.NewSource(11))

	stored := newID(rng)
	if err := idx.StoreVector(stored, randomUnitVector(rng, 16)); err != nil {
		t.Fatalf("StoreVector: %v", err)
	}

	dim, present, err := idx.StoredVectorDim(stored)
	if err != nil || !present || dim != 16 {
		t.Fatalf("stored slot: dim=%d present=%v err=%v, want 16/true/nil", dim, present, err)
	}

	dim, present, err = idx.StoredVectorDim(newID(rng))
	if err != nil || present || dim != 0 {
		t.Fatalf("missing slot: dim=%d present=%v err=%v, want 0/false/nil", dim, present, err)
	}
}

// TestSelfProbe_FindsInsertedMissesSkipped is the instrument's core contract:
// a node that reached the graph answers its own vector; a vector that was
// stored to Pebble but never graph-inserted (the silent-skip defect class)
// does NOT — even though its 0xFF slot exists.
func TestSelfProbe_FindsInsertedMissesSkipped(t *testing.T) {
	db := newTestDB(t)
	idx := hnsw.New(db, testWS())
	t.Cleanup(idx.Close) // drain persistNode goroutines before db.Close() fires
	rng := rand.New(rand.NewSource(13))
	ctx := context.Background()

	// A small populated graph.
	for i := 0; i < 20; i++ {
		id := newID(rng)
		vec := randomUnitVector(rng, 8)
		if err := idx.StoreVector(id, vec); err != nil {
			t.Fatalf("StoreVector: %v", err)
		}
		idx.Insert(id, vec)
	}

	member := newID(rng)
	memberVec := randomUnitVector(rng, 8)
	if err := idx.StoreVector(member, memberVec); err != nil {
		t.Fatalf("StoreVector: %v", err)
	}
	idx.Insert(member, memberVec)

	found, rank, err := idx.SelfProbe(ctx, member, 5)
	if err != nil {
		t.Fatalf("SelfProbe(member): %v", err)
	}
	if !found || rank != 1 {
		t.Fatalf("graph member self-probe: found=%v rank=%d, want true/1", found, rank)
	}

	// Vector stored, graph insert "skipped" — the stuck-vector shape.
	orphan := newID(rng)
	if err := idx.StoreVector(orphan, randomUnitVector(rng, 8)); err != nil {
		t.Fatalf("StoreVector: %v", err)
	}
	found, _, err = idx.SelfProbe(ctx, orphan, 5)
	if err != nil {
		t.Fatalf("SelfProbe(orphan): %v", err)
	}
	if found {
		t.Fatal("orphaned vector (stored, never inserted) self-probe: found=true, want false")
	}

	// No vector anywhere: probe reports not-found without error.
	found, _, err = idx.SelfProbe(ctx, newID(rng), 5)
	if err != nil || found {
		t.Fatalf("no-vector self-probe: found=%v err=%v, want false/nil", found, err)
	}
}
