package hnsw

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// seqID builds a deterministic, sortable 16-byte id from a sequence number.
func seqID(i int) [16]byte {
	var id [16]byte
	binary.BigEndian.PutUint64(id[8:], uint64(i)+1)
	return id
}

// jitteredUnit returns base plus scale*noise, renormalized. Unique contents
// per call (the engine write path dedupes identical vectors upstream, and
// exact ties make similarity ordering ambiguous).
func jitteredUnit(rng *rand.Rand, base []float32, scale float64) []float32 {
	v := make([]float32, len(base))
	var norm float64
	for i := range v {
		x := float64(base[i]) + scale*rng.NormFloat64()
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}

func memIndexT(t *testing.T, efC, efS int) *Index {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	ws := [8]byte{4, 2, 4, 2, 4, 2, 4, 2}
	idx := NewWithParams(db, ws, efC, efS)
	// LIFO: idx.Close (flush async persists) runs BEFORE db.Close, or the
	// persistNode goroutines panic on the closed test DB.
	t.Cleanup(func() { db.Close() })
	t.Cleanup(idx.Close)
	return idx
}

func selfFound(t *testing.T, idx *Index, id [16]byte, vec []float32, k int) bool {
	t.Helper()
	res, err := idx.Search(context.Background(), vec, k)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, r := range res {
		if r.ID == id {
			return true
		}
	}
	return false
}

// TestInsertGuaranteesInLinkAtEdgeOfSaturatedCluster is the deterministic
// reproduction of the in-link starvation verdict (2026-07-17 rare nodes,
// swing's bridge engram): a tight cluster saturates every member's layer-0
// list with sub-jitter-distance friends, then a newcomer lands at the edge —
// nearer to the cluster than to anything else, but farther from each member
// than they are to each other. Pre-fix, every selected neighbor prunes the
// newcomer's back-link in the same breath and it is born with out-edges
// only: present in the graph, invisible to Search, forever (no later write
// arrives in a quiet region). This test FAILS on the unfixed code.
func TestInsertGuaranteesInLinkAtEdgeOfSaturatedCluster(t *testing.T) {
	// efSearch >= node count makes the layer-0 beam exhaustive over the
	// reachable component: found <=> reachable, a true reachability oracle.
	idx := memIndexT(t, 200, 256)
	rng := rand.New(rand.NewSource(7))

	dim := 32
	base := make([]float32, dim)
	base[0] = 1

	// 100 members, jitter 0.001: every layer-0 list fills with ~0.000001-
	// distance friends (cosine distances scale with jitter^2).
	n := 100
	for i := 0; i < n; i++ {
		idx.Insert(seqID(i), jitteredUnit(rng, base, 0.001))
	}

	// The edge case: offset ~0.02 from the centroid — its nearest neighbors
	// are all cluster members, and every one of them holds a full list of
	// entries 400x closer than the newcomer.
	edge := make([]float32, dim)
	copy(edge, base)
	edge[1] = 0.02
	var norm float64
	for _, x := range edge {
		norm += float64(x) * float64(x)
	}
	for i := range edge {
		edge[i] = float32(float64(edge[i]) / math.Sqrt(norm))
	}
	edgeID := seqID(999)
	idx.Insert(edgeID, edge)

	// Birth contract 1: the node holds at least one layer-0 in-link.
	inlinks := 0
	idx.mu.RLock()
	for oid, node := range idx.nodes {
		if oid == edgeID {
			continue
		}
		node.mu.RLock()
		if len(node.layers) > 0 {
			for _, nb := range node.layers[0] {
				if nb == edgeID {
					inlinks++
				}
			}
		}
		node.mu.RUnlock()
	}
	idx.mu.RUnlock()
	if inlinks == 0 {
		t.Errorf("edge node born with zero layer-0 in-links: out-edges only, unreachable by construction")
	}

	// Birth contract 2 (the consequence Search-side): self-probe finds it.
	if !selfFound(t, idx, edgeID, edge, 10) {
		t.Errorf("edge node not found by its own vector at k=10: born search-blind")
	}
}

// TestDenseClusterBirthReachability is the property form: in a continuously
// saturated cluster, EVERY insert must be findable by its own vector
// immediately after it lands. (Birth contract only — a node can lose its
// in-links to later prunes; that residue is the repair sweep's territory,
// by design.)
func TestDenseClusterBirthReachability(t *testing.T) {
	idx := memIndexT(t, 200, 512)
	rng := rand.New(rand.NewSource(11))

	dim := 32
	base := make([]float32, dim)
	base[0] = 1

	for i := 0; i < 300; i++ {
		id := seqID(i)
		vec := jitteredUnit(rng, base, 0.005)
		idx.Insert(id, vec)
		if !selfFound(t, idx, id, vec, 10) {
			t.Fatalf("insert %d born unreachable (self-probe miss at k=10)", i)
		}
	}
}

// TestPinBacklinkCapAndPin exercises the pin mechanics directly: a full
// layer-0 list of strictly-nearer entries must keep the pinned id, stay at
// M0, and evict exactly the farthest non-pinned entry.
func TestPinBacklinkCapAndPin(t *testing.T) {
	idx := memIndexT(t, 200, 50)

	dim := 8
	nbVec := make([]float32, dim)
	nbVec[0] = 1
	nb := &HNSWNode{id: seqID(5000), vec: nbVec, layers: make([][][16]byte, 1)}
	idx.nodes[nb.id] = nb

	// Fill nb's layer 0 with M0 entries at increasing distance, all nearer
	// than the pinned newcomer will be.
	for i := 0; i < M0; i++ {
		fid := seqID(6000 + i)
		fvec := make([]float32, dim)
		copy(fvec, nbVec)
		fvec[1] = float32(0.001 * float64(i+1))
		var norm float64
		for _, x := range fvec {
			norm += float64(x) * float64(x)
		}
		for j := range fvec {
			fvec[j] = float32(float64(fvec[j]) / math.Sqrt(norm))
		}
		idx.nodes[fid] = &HNSWNode{id: fid, vec: fvec}
		nb.layers[0] = append(nb.layers[0], fid)
	}
	farthest := seqID(6000 + M0 - 1)

	newVec := make([]float32, dim)
	copy(newVec, nbVec)
	newVec[1] = 0.5 // much farther than every resident
	var norm float64
	for _, x := range newVec {
		norm += float64(x) * float64(x)
	}
	for j := range newVec {
		newVec[j] = float32(float64(newVec[j]) / math.Sqrt(norm))
	}
	newID := seqID(7000)
	idx.nodes[newID] = &HNSWNode{id: newID, vec: newVec}

	idx.mu.Lock()
	idx.pinBacklink(nb, newID)
	idx.mu.Unlock()

	if got := len(nb.layers[0]); got != M0 {
		t.Fatalf("pinned list length = %d, want M0 = %d", got, M0)
	}
	hasNew, hasFarthest := false, false
	for _, e := range nb.layers[0] {
		if e == newID {
			hasNew = true
		}
		if e == farthest {
			hasFarthest = true
		}
	}
	if !hasNew {
		t.Errorf("pinned id missing from the list after pinBacklink")
	}
	if hasFarthest {
		t.Errorf("farthest resident survived; the pin must evict the farthest non-pinned entry")
	}

	// Idempotence: pinning an already-present id is a no-op.
	before := append([][16]byte(nil), nb.layers[0]...)
	idx.mu.Lock()
	idx.pinBacklink(nb, newID)
	idx.mu.Unlock()
	if len(nb.layers[0]) != len(before) {
		t.Errorf("re-pin changed list length: %d -> %d", len(before), len(nb.layers[0]))
	}
}

// TestGuaranteeSurvivesReload: the pinned back-link must reach Pebble (the
// pinned neighbor rides the existing mutated-persist fan-out), so the birth
// guarantee holds across a restart.
func TestGuaranteeSurvivesReload(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()
	ws := [8]byte{8, 8, 8, 8, 8, 8, 8, 8}
	idx := NewWithParams(db, ws, 200, 256)

	rng := rand.New(rand.NewSource(13))
	dim := 32
	base := make([]float32, dim)
	base[0] = 1
	for i := 0; i < 100; i++ {
		vec := jitteredUnit(rng, base, 0.001)
		if err := idx.StoreVector(seqID(i), vec); err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
		idx.Insert(seqID(i), vec)
	}
	edge := make([]float32, dim)
	copy(edge, base)
	edge[1] = 0.02
	var norm float64
	for _, x := range edge {
		norm += float64(x) * float64(x)
	}
	for i := range edge {
		edge[i] = float32(float64(edge[i]) / math.Sqrt(norm))
	}
	edgeID := seqID(999)
	if err := idx.StoreVector(edgeID, edge); err != nil {
		t.Fatalf("store edge: %v", err)
	}
	idx.Insert(edgeID, edge)
	idx.Close() // flush every in-flight persist: disk becomes the only truth

	fresh := NewWithParams(db, ws, 200, 256)
	defer fresh.Close()
	if err := fresh.LoadFromPebble(); err != nil {
		t.Fatalf("LoadFromPebble: %v", err)
	}
	if !selfFound(t, fresh, edgeID, edge, 10) {
		t.Errorf("edge node unreachable after reload: the pinned back-link never reached Pebble")
	}
}

// TestReinsertRepairsOrphan manufactures the production defect — a node in
// the graph with out-edges only — and proves Reinsert heals it, in memory
// and across a reload, with stale higher-layer rows cleaned up.
func TestReinsertRepairsOrphan(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()
	ws := [8]byte{3, 1, 3, 1, 3, 1, 3, 1}
	idx := NewWithParams(db, ws, 200, 256)
	defer idx.Close()

	rng := rand.New(rand.NewSource(17))
	dim := 16
	base := make([]float32, dim)
	base[0] = 1
	for i := 0; i < 50; i++ {
		vec := jitteredUnit(rng, base, 0.05)
		if err := idx.StoreVector(seqID(i), vec); err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
		idx.Insert(seqID(i), vec)
	}

	// Manufacture the orphan: strip victim's id from every other node's
	// lists (all layers), and give the victim's on-disk record extra stale
	// high-layer rows so Reinsert's cleanup is observable.
	victim := seqID(25)
	vNode := idx.nodes[victim]
	if vNode == nil {
		t.Fatalf("victim missing from graph")
	}
	vVec := vNode.vec
	if victim == idx.entryPoint {
		t.Skip("victim drew the entry point; orphan construction needs a non-entry node")
	}
	idx.mu.RLock()
	for oid, node := range idx.nodes {
		if oid == victim {
			continue
		}
		node.mu.Lock()
		for l := range node.layers {
			kept := node.layers[l][:0]
			for _, nb := range node.layers[l] {
				if nb != victim {
					kept = append(kept, nb)
				}
			}
			node.layers[l] = kept
		}
		node.mu.Unlock()
	}
	idx.mu.RUnlock()

	staleLayer := uint8(9)
	if err := db.Set(keys.HNSWNodeKey(ws, victim, staleLayer), encodeNeighbors([][16]byte{seqID(1)}), pebble.Sync); err != nil {
		t.Fatalf("plant stale row: %v", err)
	}
	// Reinsert derives its cleanup bound from the in-memory layer count, so
	// the planted stale row must be visible there too.
	vNode.mu.Lock()
	for len(vNode.layers) <= int(staleLayer) {
		vNode.layers = append(vNode.layers, nil)
	}
	vNode.mu.Unlock()

	if selfFound(t, idx, victim, vVec, 10) {
		t.Fatalf("victim still reachable; orphan construction failed")
	}

	ok, err := idx.Reinsert(victim)
	if err != nil {
		t.Fatalf("Reinsert: %v", err)
	}
	if !ok {
		t.Fatalf("Reinsert reported not-found for a live node")
	}
	if !selfFound(t, idx, victim, vVec, 10) {
		t.Errorf("victim still unreachable after Reinsert")
	}

	// Disk truth: reload finds it, and the stale layer-9 row is gone.
	idx.Close()
	if _, closer, err := db.Get(keys.HNSWNodeKey(ws, victim, staleLayer)); err == nil {
		closer.Close()
		t.Errorf("stale layer-%d row survived Reinsert; it would resurface on the next LoadFromPebble", staleLayer)
	}
	fresh := NewWithParams(db, ws, 200, 256)
	defer fresh.Close()
	if err := fresh.LoadFromPebble(); err != nil {
		t.Fatalf("LoadFromPebble: %v", err)
	}
	if !selfFound(t, fresh, victim, vVec, 10) {
		t.Errorf("victim unreachable after Reinsert + reload")
	}
}

// TestReinsertRefusesEntryPoint: the entry point is reachable by
// construction; pulling it from the map would strand every Search at a nil
// entry. Reinsert must refuse.
func TestReinsertRefusesEntryPoint(t *testing.T) {
	idx := memIndexT(t, 200, 50)
	v1 := []float32{1, 0, 0, 0}
	v2 := []float32{0.9, 0.1, 0, 0}
	idx.Insert(seqID(1), v1)
	idx.Insert(seqID(2), v2)

	idx.mu.RLock()
	entry := idx.entryPoint
	idx.mu.RUnlock()

	ok, err := idx.Reinsert(entry)
	if err == nil {
		t.Fatalf("Reinsert(entryPoint) succeeded (ok=%v); it must refuse", ok)
	}
	if !selfFound(t, idx, entry, idx.nodes[entry].vec, 2) {
		t.Errorf("entry point vanished after refused Reinsert")
	}
}

// TestNodeIDsSortedSnapshot: paging cursors need a stable, sorted order.
func TestNodeIDsSortedSnapshot(t *testing.T) {
	idx := memIndexT(t, 200, 50)
	rng := rand.New(rand.NewSource(19))
	dim := 8
	base := make([]float32, dim)
	base[0] = 1
	for _, i := range []int{5, 3, 9, 1, 7} {
		idx.Insert(seqID(i), jitteredUnit(rng, base, 0.05))
	}
	ids := idx.NodeIDs()
	if len(ids) != 5 {
		t.Fatalf("NodeIDs returned %d ids, want 5", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if string(ids[i-1][:]) >= string(ids[i][:]) {
			t.Fatalf("NodeIDs not strictly sorted at %d", i)
		}
	}
}
