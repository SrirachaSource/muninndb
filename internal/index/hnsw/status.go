package hnsw

import (
	"context"
	"errors"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// NodeStatus reports a node's in-memory graph state: whether it is present in
// the graph, its neighbor count per layer, and whether it is tombstoned.
// Read-only; safe on a live index.
func (idx *Index) NodeStatus(id [16]byte) (present bool, edgesPerLayer []int, tombstoned bool) {
	_, tombstoned = idx.deleted.Load(id)
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	node, ok := idx.nodes[id]
	if !ok {
		return false, nil, tombstoned
	}
	node.mu.RLock()
	edgesPerLayer = make([]int, len(node.layers))
	for l, neighbors := range node.layers {
		edgesPerLayer[l] = len(neighbors)
	}
	node.mu.RUnlock()
	return true, edgesPerLayer, tombstoned
}

// StoredVectorDim reads the persisted 0xFF vector slot for id and reports its
// dimension. present=false means no slot row exists in Pebble.
func (idx *Index) StoredVectorDim(id [16]byte) (dim int, present bool, err error) {
	key := keys.HNSWNodeKey(idx.ws, id, 0xFF)
	buf, closer, err := idx.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	dim = len(buf) / 4
	closer.Close()
	return dim, true, nil
}

// SelfProbe answers "can graph search find this node from its own vector?" —
// the reachability test the storage-row audits cannot perform. The probe
// vector comes from the in-memory node if present, else from the persisted
// 0xFF slot. found reports whether id appears in the top-k results; rank is
// its 1-based position (0 when not found). Read-only.
func (idx *Index) SelfProbe(ctx context.Context, id [16]byte, k int) (found bool, rank int, err error) {
	idx.mu.RLock()
	node := idx.nodes[id]
	var vec []float32
	if node != nil {
		vec = node.vec
	}
	idx.mu.RUnlock()

	if len(vec) == 0 {
		key := keys.HNSWNodeKey(idx.ws, id, 0xFF)
		buf, closer, gerr := idx.db.Get(key)
		if gerr != nil {
			if errors.Is(gerr, pebble.ErrNotFound) {
				return false, 0, nil // no vector anywhere — nothing to probe with
			}
			return false, 0, gerr
		}
		vec = decodeVector(buf)
		closer.Close()
	}
	if len(vec) == 0 {
		return false, 0, nil
	}

	results, err := idx.Search(ctx, vec, k)
	if err != nil {
		return false, 0, err
	}
	for i, r := range results {
		if r.ID == id {
			return true, i + 1, nil
		}
	}
	return false, 0, nil
}

// NodeStatus delegates to the per-vault Index (loading it from Pebble if this
// is the first access since startup, same as Search).
func (r *Registry) NodeStatus(ws [8]byte, id [16]byte) (present bool, edgesPerLayer []int, tombstoned bool) {
	return r.getOrCreate(ws).NodeStatus(id)
}

// StoredVectorDim delegates to the per-vault Index.
func (r *Registry) StoredVectorDim(ws [8]byte, id [16]byte) (dim int, present bool, err error) {
	return r.getOrCreate(ws).StoredVectorDim(id)
}

// SelfProbe delegates to the per-vault Index.
func (r *Registry) SelfProbe(ctx context.Context, ws [8]byte, id [16]byte, k int) (found bool, rank int, err error) {
	return r.getOrCreate(ws).SelfProbe(ctx, id, k)
}
