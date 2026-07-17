package hnsw

import (
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// RebuildGraph rebuilds one vault's HNSW graph from its stored vectors.
//
// WHY THIS EXISTS: before the back-link persistence fix (2026-07-17), Insert
// persisted only the new node's neighbor lists, so every graph written by the
// old code is a forward-only DAG on disk -- restarts strand everything inserted
// after the oldest reachable core. Fixing Insert stops the rot but cannot heal
// stored graphs (the trading vault measured 13 disconnected components,
// largest 38.8%). The repair is to drop the graph and re-insert every stored
// vector through the fixed path, which persists both directions.
//
// The caller must guarantee exclusive access to db (server stopped). Vectors
// (slot 0xFF) are the source of truth and are not touched; only neighbor-list
// records (slot != 0xFF) are deleted and rewritten. Engrams whose vectors were
// never stored are unaffected -- they were never semantically searchable.
//
// Returns the number of vectors re-inserted.
func RebuildGraph(db *pebble.DB, ws [8]byte) (int, error) {
	lower := make([]byte, 9)
	lower[0] = 0x07
	copy(lower[1:], ws[:])
	wsPlus, err := keys.IncrementWSPrefix(ws)
	if err != nil {
		return 0, fmt.Errorf("hnsw rebuild: ws bound: %w", err)
	}
	upper := make([]byte, 9)
	upper[0] = 0x07
	copy(upper[1:], wsPlus[:])

	iter, err := db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return 0, fmt.Errorf("hnsw rebuild: iter: %w", err)
	}

	// Pass 1: collect vectors (0xFF) and the stale graph keys (everything else).
	type entry struct {
		id  [16]byte
		vec []float32
	}
	var entries []entry
	var staleKeys [][]byte
	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()
		if len(key) < 26 {
			continue
		}
		if key[25] == 0xFF {
			var id [16]byte
			copy(id[:], key[9:25])
			entries = append(entries, entry{id: id, vec: decodeVector(iter.Value())})
			continue
		}
		staleKeys = append(staleKeys, append([]byte(nil), key...))
	}
	if err := iter.Error(); err != nil {
		iter.Close()
		return 0, fmt.Errorf("hnsw rebuild: scan: %w", err)
	}
	iter.Close()

	// Pass 2: drop the stale graph. Doing this BEFORE re-inserting matters:
	// the rebuilt node may land at a lower random level than before, and a
	// leftover higher-layer record would resurface on the next LoadFromPebble.
	batch := db.NewBatch()
	for _, k := range staleKeys {
		if err := batch.Delete(k, nil); err != nil {
			return 0, fmt.Errorf("hnsw rebuild: delete: %w", err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return 0, fmt.Errorf("hnsw rebuild: commit deletes: %w", err)
	}

	// Pass 3: re-insert every vector in ULID (= insertion-time) order through
	// the fixed Insert, which persists back-links. Close flushes the persists.
	idx := New(db, ws)
	for _, e := range entries {
		if len(e.vec) == 0 {
			continue
		}
		idx.Insert(e.id, e.vec)
	}
	idx.Close()

	return len(entries), nil
}

