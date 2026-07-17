package hnsw

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestRebuildGraphHealsFragmentedGraph proves the repair for graphs written by
// the pre-fix Insert (back-links never persisted).
//
// Setup fabricates the damage directly on disk: N vectors stored, but the
// persisted neighbor lists form a forward-only chain missing most links --
// the shape the old code left after era boundaries. Before rebuild, a reloaded
// index cannot find late nodes by their own vectors. After RebuildGraph, every
// node must be findable by its own vector.
func TestRebuildGraphHealsFragmentedGraph(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	ws := [8]byte{7, 7, 7, 7, 7, 7, 7, 7}
	const n = 40

	// Deterministic pseudo-random unit vectors. NOT one-hot: mutually orthogonal
	// vectors give greedy HNSW routing zero gradient (every neighbor equidistant)
	// and search legitimately cannot navigate -- that failure mode is not the one
	// under test. Real embeddings are clustered; these are too.
	rng := uint64(0x9E3779B97F4A7C15)
	next := func() float32 {
		rng ^= rng << 13
		rng ^= rng >> 7
		rng ^= rng << 17
		return float32(int64(rng%2000)-1000) / 1000.0
	}
	ids := make([][16]byte, n)
	vecs := make([][]float32, n)
	for i := 0; i < n; i++ {
		ids[i] = [16]byte{byte(i + 1), 0xAB}
		v := make([]float32, 16)
		for d := range v {
			v[d] = next()
		}
		vecs[i] = v
	}

	// CONTROL: a cleanly built index on these exact vectors must find every
	// node by its own vector -- otherwise the vector set (not the rebuild) is
	// what any later failure measures.
	ctrlDB, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open control pebble: %v", err)
	}
	ctrl := New(ctrlDB, ws)
	for i := 0; i < n; i++ {
		if err := ctrl.StoreVector(ids[i], vecs[i]); err != nil {
			t.Fatalf("control store %d: %v", i, err)
		}
		ctrl.Insert(ids[i], vecs[i])
	}
	ctrlFound := 0
	for i := 0; i < n; i++ {
		res, err := ctrl.Search(context.Background(), vecs[i], 3)
		if err != nil {
			t.Fatalf("control search %d: %v", i, err)
		}
		for _, r := range res {
			if r.ID == ids[i] {
				ctrlFound++
				break
			}
		}
	}
	ctrl.Close()
	ctrlDB.Close()
	if ctrlFound != n {
		t.Fatalf("control failed: fresh index finds only %d/%d of its own vectors -- "+
			"fixture vectors are unnavigable, rebuild result would be meaningless", ctrlFound, n)
	}

	// Store every vector (0xFF slots) -- the truth that survives.
	seed := New(db, ws)
	for i := 0; i < n; i++ {
		if err := seed.StoreVector(ids[i], vecs[i]); err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
	}
	// Fabricate the pre-fix damage: each node's ONLY persisted edge points at
	// node 0 (forward-only star into the oldest core; no back-links anywhere).
	for i := 1; i < n; i++ {
		key := keys.HNSWNodeKey(ws, ids[i], 0)
		if err := db.Set(key, encodeNeighbors([][16]byte{ids[0]}), pebble.Sync); err != nil {
			t.Fatalf("seed edge %d: %v", i, err)
		}
	}
	if err := db.Set(keys.HNSWNodeKey(ws, ids[0], 0), encodeNeighbors(nil), pebble.Sync); err != nil {
		t.Fatalf("seed node0: %v", err)
	}

	countFindable := func(label string) int {
		idx := New(db, ws)
		if err := idx.LoadFromPebble(); err != nil {
			t.Fatalf("%s: load: %v", label, err)
		}
		found := 0
		for i := 0; i < n; i++ {
			res, err := idx.Search(context.Background(), vecs[i], 3)
			if err != nil {
				t.Fatalf("%s: search %d: %v", label, i, err)
			}
			for _, r := range res {
				if r.ID == ids[i] {
					found++
					break
				}
			}
		}
		return found
	}

	before := countFindable("before")
	if before == n {
		t.Fatalf("fixture failed to reproduce the damage: all %d nodes findable "+
			"before rebuild -- the test would prove nothing", n)
	}

	got, err := RebuildGraph(db, ws)
	if err != nil {
		t.Fatalf("RebuildGraph: %v", err)
	}
	if got != n {
		t.Errorf("RebuildGraph reinserted %d vectors; want %d", got, n)
	}

	after := countFindable("after")
	if after != n {
		t.Errorf("after rebuild only %d/%d nodes findable by their own vector "+
			"(before: %d/%d) -- rebuild did not restore connectivity", after, n, before, n)
	}
	t.Logf("findable by own vector: before=%d/%d after=%d/%d", before, n, after, n)
}

// TestRebuildGraphLeavesOtherVaultsUntouched pins vault isolation of the repair.
func TestRebuildGraphLeavesOtherVaultsUntouched(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	wsA := [8]byte{1}
	wsB := [8]byte{2}

	b := New(db, wsB)
	idB := [16]byte{0xB7}
	vecB := []float32{1, 2, 3, 4}
	if err := b.StoreVector(idB, vecB); err != nil {
		t.Fatalf("store B: %v", err)
	}
	b.Insert(idB, vecB)
	b.Close()

	// Snapshot B's keys before rebuilding A.
	dumpB := func() map[string]string {
		out := map[string]string{}
		lower := make([]byte, 9)
		lower[0] = 0x07
		copy(lower[1:], wsB[:])
		wsPlus, _ := keys.IncrementWSPrefix(wsB)
		upper := make([]byte, 9)
		upper[0] = 0x07
		copy(upper[1:], wsPlus[:])
		iter, err := db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
		if err != nil {
			t.Fatalf("iter: %v", err)
		}
		defer iter.Close()
		for iter.First(); iter.Valid(); iter.Next() {
			out[fmt.Sprintf("%x", iter.Key())] = fmt.Sprintf("%x", iter.Value())
		}
		return out
	}
	beforeB := dumpB()

	a := New(db, wsA)
	idA := [16]byte{0xA7}
	vecA := []float32{4, 3, 2, 1}
	if err := a.StoreVector(idA, vecA); err != nil {
		t.Fatalf("store A: %v", err)
	}
	a.Insert(idA, vecA)
	a.Close()

	if _, err := RebuildGraph(db, wsA); err != nil {
		t.Fatalf("RebuildGraph A: %v", err)
	}

	afterB := dumpB()
	if len(afterB) != len(beforeB) {
		t.Fatalf("vault B key count changed: %d -> %d", len(beforeB), len(afterB))
	}
	for k, v := range beforeB {
		if afterB[k] != v {
			t.Errorf("vault B key %s changed by rebuilding vault A", k)
		}
	}
}
