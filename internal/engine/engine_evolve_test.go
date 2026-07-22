package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/plugin"
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

// TestEvolve_CarriesEntityLinksAndDigestEntities proves the issue-80 enrich-half
// fix: caller-declared entities describe the MEMORY, not the ULID, so an evolve
// must carry the entity links AND the DigestEntities flag to the new engram.
// Without the carry, the evolved engram reads flags=0 and retroactive enrichment
// re-extracts entities from the new content, replacing the caller's assertions
// with model guesses.
func TestEvolve_CarriesEntityLinksAndDigestEntities(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Original", Content: "old content",
		Summary: "caller summary",
		Entities: []mbp.InlineEntity{
			{Name: "validEntityTypes", Type: "concept"},
			{Name: "muninn", Type: "tool"},
		},
	})
	require.NoError(t, err)
	oldULID, err := storage.ParseULID(resp.ID)
	require.NoError(t, err)

	// POWER CHECK: the write path must have flagged the old engram, or the
	// carry assertion below could never fail for the right reason.
	oldFlags, err := eng.store.GetDigestFlags(ctx, plugin.ULID(oldULID))
	require.NoError(t, err)
	require.NotZero(t, oldFlags&plugin.DigestEntities, "precondition: write must set DigestEntities")

	newID, err := eng.Evolve(ctx, "test", resp.ID, "new content", "update", nil)
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")

	// Entity links carried old -> new.
	var carried []string
	require.NoError(t, eng.store.ScanEngramEntities(ctx, ws, newID, func(name string) error {
		carried = append(carried, name)
		return nil
	}))
	assert.ElementsMatch(t, []string{"validEntityTypes", "muninn"}, carried,
		"caller-declared entity links must carry across evolve")

	// DigestEntities carried so retroactive enrichment does not re-extract.
	newFlags, err := eng.store.GetDigestFlags(ctx, plugin.ULID(newID))
	require.NoError(t, err)
	assert.NotZero(t, newFlags&plugin.DigestEntities,
		"DigestEntities must carry across evolve or re-extraction rewrites the entities")
}

// TestEvolve_CarriesClassificationNotSummarizedFlag pins the carry SET: the
// classification (MemoryType/TypeLabel + DigestClassified) describes identity
// and carries; DigestSummarized does NOT carry because the content changed and
// the summary must re-derive.
func TestEvolve_CarriesClassificationNotSummarizedFlag(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Original", Content: "old content",
		MemoryType: uint8(storage.TypeIssue), TypeLabel: "engine_defect",
	})
	require.NoError(t, err)
	oldULID, err := storage.ParseULID(resp.ID)
	require.NoError(t, err)

	// Set DigestClassified + DigestSummarized on the OLD engram by hand so both
	// carry assertions below have power (a flag that was never set cannot fail
	// to carry).
	// "pebble: not found" means no flags written yet — treat as 0, same as the
	// production callers do.
	existing, _ := eng.store.GetDigestFlags(ctx, plugin.ULID(oldULID))
	require.NoError(t, eng.store.SetDigestFlag(ctx, oldULID,
		existing|plugin.DigestClassified|plugin.DigestSummarized))

	newID, err := eng.Evolve(ctx, "test", resp.ID, "new content", "update", nil)
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	newEng, err := eng.store.GetEngram(ctx, ws, newID)
	require.NoError(t, err)
	require.NotNil(t, newEng)

	assert.Equal(t, storage.TypeIssue, newEng.MemoryType, "MemoryType must carry across evolve")
	assert.Equal(t, "engine_defect", newEng.TypeLabel, "TypeLabel must carry across evolve")

	newFlags, err := eng.store.GetDigestFlags(ctx, plugin.ULID(newID))
	require.NoError(t, err)
	assert.NotZero(t, newFlags&plugin.DigestClassified, "DigestClassified must carry across evolve")
	assert.Zero(t, newFlags&plugin.DigestSummarized,
		"DigestSummarized must NOT carry: content changed, summary re-derives")
}
