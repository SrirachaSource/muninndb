package hnsw

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

// TestLoadFromPebbleIsolatesVaults pins the invariant that an Index for vault A
// loads ONLY vault A's vectors.
//
// WHY: LoadFromPebble scans the key range [0x07, 0x08) -- the bare prefix byte,
// with NO workspace bound -- while HNSWNodeKey writes 0x07|ws(8)|id(16)|slot(1).
// So the iterator walks EVERY vault's nodes, and the loop derives the id from
// key[9:25] without ever comparing key[1:9] against idx.ws.
//
// Consequences if unfixed:
//   - Every per-vault index holds every OTHER vault's vectors (cross-vault
//     search contamination -- vault A can surface vault B's engram ids).
//   - Memory is multiplied by the vault count: on the live floor that is
//     ~38.4k vectors x 3072 dims x 4B = ~472MB PER INDEX, x17 vaults = ~8GB.
//
// This test FAILS on the current code. That failure IS the reproduction.
func TestLoadFromPebbleIsolatesVaults(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	wsA := [8]byte{1, 1, 1, 1, 1, 1, 1, 1}
	wsB := [8]byte{2, 2, 2, 2, 2, 2, 2, 2}

	idA := [16]byte{0xA1}
	idB := [16]byte{0xB1}

	vecA := []float32{1, 0, 0, 0}
	vecB := []float32{0, 1, 0, 0}

	// Populate vault A with exactly one vector, vault B with exactly one.
	a := New(db, wsA)
	if err := a.StoreVector(idA, vecA); err != nil {
		t.Fatalf("store A: %v", err)
	}
	a.Insert(idA, vecA)
	a.Close()

	b := New(db, wsB)
	if err := b.StoreVector(idB, vecB); err != nil {
		t.Fatalf("store B: %v", err)
	}
	b.Insert(idB, vecB)
	b.Close()

	// Load a FRESH index for vault A only.
	fresh := New(db, wsA)
	if err := fresh.LoadFromPebble(); err != nil {
		t.Fatalf("LoadFromPebble: %v", err)
	}

	got := fresh.Len()
	if got != 1 {
		t.Errorf("vault A index loaded %d nodes; want exactly 1 (its own). "+
			"Extra nodes are OTHER vaults' vectors -- LoadFromPebble scans "+
			"[0x07,0x08) with no workspace bound.", got)
	}

	// The contamination is not merely a count: vault B's id must not be reachable
	// from a search against vault A.
	res, err := fresh.Search(context.Background(), vecB, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, r := range res {
		if r.ID == idB {
			t.Errorf("vault A search returned vault B's engram id %x -- cross-vault leak", r.ID)
		}
	}
}
