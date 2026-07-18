package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// testEnvWithHNSW mirrors testEnv but wires a real HNSW registry so the
// vector-status surface has a live graph to inspect.
func testEnvWithHNSW(t *testing.T) (*Engine, *hnswpkg.Registry, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-vecstatus-test-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	registry := hnswpkg.NewRegistry(db)
	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, activation.NewHNSWAdapter(registry), embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	eng := NewEngine(EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder, HNSWRegistry: registry})

	return eng, registry, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

func testVec(dim int, seed float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = seed + float32(i)*0.01
	}
	return v
}

// TestVectorStatus_EmbeddedVsUnembedded locks the instrument's core contract:
// a client-embedded write reads graph-present and self-probe-reachable; an
// unembedded write reads absent everywhere, with no error either way.
func TestVectorStatus_EmbeddedVsUnembedded(t *testing.T) {
	eng, _, cleanup := testEnvWithHNSW(t)
	defer cleanup()
	ctx := context.Background()

	embedded, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "vs-test",
		Concept:   "embedded engram",
		Content:   "carries a client-supplied vector",
		Embedding: testVec(8, 0.5),
	})
	if err != nil {
		t.Fatalf("Write(embedded): %v", err)
	}
	bare, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "vs-test",
		Concept: "bare engram",
		Content: "no vector anywhere",
	})
	if err != nil {
		t.Fatalf("Write(bare): %v", err)
	}

	status, err := eng.VectorStatus(ctx, "vs-test", embedded.ID, true, 5)
	if err != nil {
		t.Fatalf("VectorStatus(embedded): %v", err)
	}
	if !status.GraphInMemory {
		t.Error("embedded engram: GraphInMemory=false, want true")
	}
	if !status.VecSlotPresent || status.VecSlotDim != 8 {
		t.Errorf("embedded engram: VecSlotPresent=%v VecSlotDim=%d, want true/8", status.VecSlotPresent, status.VecSlotDim)
	}
	if !status.FlagEmbedded {
		t.Error("embedded engram: FlagEmbedded=false, want true (client-embedding path sets DigestEmbed)")
	}
	if !status.ProbeRequested || !status.ProbeFound {
		t.Errorf("embedded engram: probe requested=%v found=%v, want true/true", status.ProbeRequested, status.ProbeFound)
	}

	status, err = eng.VectorStatus(ctx, "vs-test", bare.ID, true, 5)
	if err != nil {
		t.Fatalf("VectorStatus(bare): %v", err)
	}
	if status.GraphInMemory || status.VecSlotPresent || status.RowPresent {
		t.Errorf("bare engram: graph=%v slot=%v row=%v, want all false",
			status.GraphInMemory, status.VecSlotPresent, status.RowPresent)
	}
	if status.ProbeFound {
		t.Error("bare engram: ProbeFound=true, want false (nothing to probe with)")
	}
}

// TestVectorStatus_LookupErrorsLoud: malformed ID errors at parse; a
// well-formed unknown ID returns ErrEngramNotFound — never a zeroed result.
func TestVectorStatus_LookupErrorsLoud(t *testing.T) {
	eng, _, cleanup := testEnvWithHNSW(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.VectorStatus(ctx, "vs-test", "not-a-ulid", false, 0); err == nil {
		t.Fatal("malformed ID: expected parse error, got nil")
	}
	_, err := eng.VectorStatus(ctx, "vs-test", "01HNKZ5F00000000000000000A", false, 0)
	if !errors.Is(err, ErrEngramNotFound) {
		t.Fatalf("unknown ID: got err=%v, want ErrEngramNotFound", err)
	}
}

// TestVaultVectorAudit_ConsistentVault: a vault whose stores agree reports
// matching counts and empty divergence lists.
func TestVaultVectorAudit_ConsistentVault(t *testing.T) {
	eng, _, cleanup := testEnvWithHNSW(t)
	defer cleanup()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:     "audit-test",
			Concept:   fmt.Sprintf("vectored %d", i),
			Content:   fmt.Sprintf("engram number %d with a client vector — distinct content so write-dedup keeps all three", i),
			Embedding: testVec(8, float32(i)),
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "audit-test",
		Concept: "bare",
		Content: "engram without a vector",
	}); err != nil {
		t.Fatalf("Write(bare): %v", err)
	}

	audit, err := eng.VaultVectorAudit(ctx, "audit-test", 0)
	if err != nil {
		t.Fatalf("VaultVectorAudit: %v", err)
	}
	if audit.Total != 4 {
		t.Errorf("Total=%d, want 4", audit.Total)
	}
	if audit.GraphNodes != 3 {
		t.Errorf("GraphNodes=%d, want 3", audit.GraphNodes)
	}
	if len(audit.LabelNoRow) != 0 || len(audit.RowNoGraph) != 0 || len(audit.Failed0x80) != 0 {
		t.Errorf("divergence lists non-empty on consistent vault: label_no_row=%v row_no_graph=%v failed=%v",
			audit.LabelNoRow, audit.RowNoGraph, audit.Failed0x80)
	}
	if audit.SampleCap != 50 {
		t.Errorf("SampleCap=%d, want default 50", audit.SampleCap)
	}
}
