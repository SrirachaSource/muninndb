package engine

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestActivate_ScoringFusionAppliesWithExplicitWeights is the regression test
// for the fusion-knob fiction (2026-07-18): the ScoringFusion wiring lived only
// in the no-explicit-weights branch, so any recall carrying explicit weights —
// which is how every mode preset (semantic/recent/deep) arrives — silently
// ignored the vault's scoring_fusion config. The knob must change scoring for
// explicit-weights requests too.
func TestActivate_ScoringFusionAppliesWithExplicitWeights(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-fusion-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)

	as := auth.NewStore(db)
	const vault = "fusionvault"
	if err := as.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	eng := NewEngine(EngineConfig{Store: store, AuthStore: as, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})
	defer func() {
		eng.Stop()
		store.Close()
	}()

	ctx := context.Background()

	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "fusion knob test",
		Content: "testing that the scoring fusion knob reaches explicit weight recalls",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitFTS(t, eng)

	// Explicit weights — the shape every mode preset produces.
	activate := func() float32 {
		resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
			Vault:      vault,
			Context:    []string{"fusion knob test"},
			MaxResults: 10,
			Threshold:  0.0001,
			Weights: &mbp.Weights{
				SemanticSimilarity: 0.6,
				FullTextRelevance:  0.4,
			},
		})
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if len(resp.Activations) == 0 {
			t.Fatal("expected at least one activation")
		}
		return resp.Activations[0].Score
	}

	// Default fusion (ACT-R path).
	actrScore := activate()

	// Flip the vault's fusion knob to RRF and repeat the identical request.
	rrf := "rrf"
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vault,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{ScoringFusion: &rrf},
	}); err != nil {
		t.Fatalf("SetVaultConfig(rrf): %v", err)
	}

	rrfScore := activate()

	// Before the fix these were byte-identical (knob ignored). RRF scores are
	// rank-based (~1/(k+rank)) and cannot coincide with the ACT-R composite.
	if actrScore == rrfScore {
		t.Fatalf("scoring_fusion knob had no effect on an explicit-weights recall: score %v under both configs", actrScore)
	}
}
