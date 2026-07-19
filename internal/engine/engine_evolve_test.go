package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func TestEvolve_AtomicBatch_OldSoftDeletedNewReadable(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Original", Content: "old content",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	newID, err := eng.Evolve(ctx, "test", resp.ID, "new content", "update", nil)
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}

	ws := eng.store.ResolveVaultPrefix("test")
	oldULID, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID old: %v", err)
	}

	// Old engram must be soft-deleted.
	old, err := eng.store.GetEngram(ctx, ws, oldULID)
	if err != nil {
		t.Fatalf("GetEngram old: %v", err)
	}
	if old == nil {
		t.Fatal("old engram not found after Evolve")
	}
	if old.State != storage.StateSoftDeleted {
		t.Errorf("old engram state = %v, want StateSoftDeleted", old.State)
	}

	// New engram must be readable and active.
	newEng, err := eng.store.GetEngram(ctx, ws, newID)
	if err != nil {
		t.Fatalf("GetEngram new: %v", err)
	}
	if newEng == nil {
		t.Fatal("new engram not found after Evolve")
	}
	if newEng.State != storage.StateActive {
		t.Errorf("new engram state = %v, want StateActive", newEng.State)
	}

	// Verify supersedes association was written.
	assocMap, err := eng.store.GetAssociations(ctx, ws, []storage.ULID{newID}, 10)
	require.NoError(t, err)
	assocs := assocMap[newID]
	require.Len(t, assocs, 1, "supersedes association must exist")
	assert.Equal(t, oldULID, assocs[0].TargetID, "association must point to old engram")
	assert.Equal(t, storage.RelSupersedes, assocs[0].RelType, "association type must be RelSupersedes")
}

// The "title half of Bucket C" (2026-07-18): evolve hardcoded the new concept
// to old + " (evolved)", so a title-lint repair could never actually change a
// title and stacked evolutions grew "(evolved) (evolved)" tails.
func TestEvolveWithConcept_CallerTitleWins(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Bad Lint Title", Content: "original content",
	})
	require.NoError(t, err)

	newID, err := eng.EvolveWithConcept(ctx, "test", resp.ID,
		"repaired content", "title repair", "Clean Honest Title", nil)
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	newEng, err := eng.store.GetEngram(ctx, ws, newID)
	require.NoError(t, err)
	require.NotNil(t, newEng)
	assert.Equal(t, "Clean Honest Title", newEng.Concept)
}

func TestEvolve_EmptyConceptKeepsAutoDerivation(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Original", Content: "old content two",
	})
	require.NoError(t, err)

	newID, err := eng.Evolve(ctx, "test", resp.ID, "new content two", "update", nil)
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	newEng, err := eng.store.GetEngram(ctx, ws, newID)
	require.NoError(t, err)
	require.NotNil(t, newEng)
	assert.Equal(t, "Original (evolved)", newEng.Concept)
}
