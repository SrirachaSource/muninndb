package hnsw

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// NodeIDs returns a sorted snapshot of every node id in the graph. IDs are
// ULIDs, so the sort is insertion-time order — a stable cursor for paged
// sweeps. Read-only.
func (idx *Index) NodeIDs() [][16]byte {
	idx.mu.RLock()
	ids := make([][16]byte, 0, len(idx.nodes))
	for id := range idx.nodes {
		ids = append(ids, id)
	}
	idx.mu.RUnlock()
	sort.Slice(ids, func(a, b int) bool { return bytes.Compare(ids[a][:], ids[b][:]) < 0 })
	return ids
}

// Reinsert pulls an existing node out of the graph and inserts it again
// through the current Insert path, which carries the layer-0 in-link
// guarantee. This is the repair for nodes born unreachable (zero in-links):
// old in-links to the id, where any exist, stay valid — the id returns to the
// map — and the fresh wiring adds the guaranteed one.
//
// The entry point is refused: an unreachable node can never BE the entry
// point (search starts there), so asking to repair it is a caller error —
// and removing it from the map would strand every Search at a nil entry.
//
// Between the map delete and the re-insert there is a sub-millisecond window
// where a concurrent Search skips the id (nil node — already handled). The
// sweep is admin-triggered; the window is accepted and documented.
func (idx *Index) Reinsert(id [16]byte) (bool, error) {
	idx.mu.Lock()
	node := idx.nodes[id]
	if node == nil {
		idx.mu.Unlock()
		return false, nil
	}
	if id == idx.entryPoint {
		idx.mu.Unlock()
		return false, fmt.Errorf("hnsw reinsert: %x is the entry point (reachable by construction)", id)
	}
	vec := node.vec
	oldLayers := len(node.layers)
	delete(idx.nodes, id)
	idx.mu.Unlock()

	// Flush in-flight persists of the OLD node object before deleting its
	// rows: an async persistNode scheduled by an earlier Insert (this node as
	// a mutated neighbor) can otherwise land AFTER the delete below and
	// resurrect stale layer rows on disk. The node is already out of the map,
	// so no NEW persist can adopt it while we wait.
	idx.persistWg.Wait()

	// Drop the stale persisted layer rows synchronously BEFORE re-inserting:
	// the node may come back at a lower random level, and a leftover
	// higher-layer row would resurface on the next LoadFromPebble (the
	// RebuildGraph lesson). The 0xFF vector slot is source of truth and is
	// not touched.
	batch := idx.db.NewBatch()
	for l := 0; l < oldLayers; l++ {
		if err := batch.Delete(keys.HNSWNodeKey(idx.ws, id, uint8(l)), nil); err != nil {
			batch.Close()
			return false, fmt.Errorf("hnsw reinsert: delete stale row: %w", err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return false, fmt.Errorf("hnsw reinsert: commit deletes: %w", err)
	}

	idx.Insert(id, vec)
	return true, nil
}

// NodeIDs delegates to the per-vault Index (loading it from Pebble if this is
// the first access since startup, same as Search).
func (r *Registry) NodeIDs(ws [8]byte) [][16]byte {
	return r.getOrCreate(ws).NodeIDs()
}

// Reinsert delegates to the per-vault Index.
func (r *Registry) Reinsert(ws [8]byte, id [16]byte) (bool, error) {
	return r.getOrCreate(ws).Reinsert(id)
}
