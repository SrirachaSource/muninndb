package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/stretchr/testify/require"
)

// TestEntityBoost_CapBoundsAccumulation verifies that an entity-dense engram
// cannot accumulate more than entityBoostCap of total boost. Regression test
// for the recall scale wart (2026-07-18): working-memory stubs sharing 15-22
// entities with the top seeds piled up scores of 2.2-3.3 (exact multiples of
// entityBoostFactor) and outranked honestly-scored content whose ACT-R
// composites top out around 1.5.
func TestEntityBoost_CapBoundsAccumulation(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-cap-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	entities := []string{"AAPL", "MSFT", "NVDA", "TSLA"}

	// Seed engram A — linked to all four entities.
	engramA := &storage.Engram{
		Concept:    "seed",
		Content:    "Canonical thesis mentioning several tickers",
		Confidence: 0.9,
	}
	idA, err := eng.store.WriteEngram(ctx, ws, engramA)
	require.NoError(t, err)

	// Spam engram S — shares ALL four entities with A. Uncapped it would
	// accumulate 4 × entityBoostFactor = 0.60; the cap must hold it at 0.30.
	engramS := &storage.Engram{
		Concept:    "stub",
		Content:    "working_memory fragment namedropping every ticker",
		Confidence: 0.7,
	}
	idS, err := eng.store.WriteEngram(ctx, ws, engramS)
	require.NoError(t, err)

	for _, name := range entities {
		err = eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
			Name:   name,
			Type:   "ticker",
			Source: "inline",
		}, "inline")
		require.NoError(t, err)
		require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idA, name))
		require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idS, name))
	}

	fullA, err := eng.store.GetEngram(ctx, ws, idA)
	require.NoError(t, err)
	require.NotNil(t, fullA)

	// S is NOT in the BFS results — it enters purely via entity boost.
	initial := []activation.ScoredEngram{{Engram: fullA, Score: 0.8}}
	boosted := eng.applyEntityBoost(ctx, ws, initial)

	var scoreA, scoreS float64
	var foundS bool
	for _, r := range boosted {
		switch r.Engram.ID {
		case idA:
			scoreA = r.Score
		case idS:
			scoreS = r.Score
			foundS = true
		}
	}

	require.True(t, foundS, "spam engram should still be surfaced (boost works)")
	require.InDelta(t, entityBoostCap, scoreS, 1e-9,
		"appended engram's accumulated boost must stop at entityBoostCap, not 4×factor")
	require.Greater(t, scoreA, scoreS,
		"honestly-scored seed must outrank the boost-only engram")
}

// TestEntityBoost_CapAppliesToExistingResults verifies the cap also bounds
// boost added to engrams that were already in the BFS result set.
func TestEntityBoost_CapAppliesToExistingResults(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-cap-existing-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	entities := []string{"SPY", "QQQ", "IWM", "DIA"}

	engramA := &storage.Engram{Concept: "seed", Content: "index thesis", Confidence: 0.9}
	idA, err := eng.store.WriteEngram(ctx, ws, engramA)
	require.NoError(t, err)

	engramB := &storage.Engram{Concept: "member", Content: "already-scored result", Confidence: 0.8}
	idB, err := eng.store.WriteEngram(ctx, ws, engramB)
	require.NoError(t, err)

	for _, name := range entities {
		err = eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
			Name:   name,
			Type:   "ticker",
			Source: "inline",
		}, "inline")
		require.NoError(t, err)
		require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idA, name))
		require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idB, name))
	}

	fullA, err := eng.store.GetEngram(ctx, ws, idA)
	require.NoError(t, err)
	fullB, err := eng.store.GetEngram(ctx, ws, idB)
	require.NoError(t, err)

	const baseB = 0.4
	initial := []activation.ScoredEngram{
		{Engram: fullA, Score: 0.8},
		{Engram: fullB, Score: baseB},
	}
	boosted := eng.applyEntityBoost(ctx, ws, initial)

	for _, r := range boosted {
		if r.Engram.ID == idB {
			require.InDelta(t, baseB+entityBoostCap, r.Score, 1e-9,
				"existing result's boost must be capped at base + entityBoostCap")
			return
		}
	}
	t.Fatal("engram B missing from boosted results")
}
