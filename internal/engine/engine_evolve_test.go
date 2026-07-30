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

// TestEvolve_RefusesAlreadySupersededEngram is the fork guard: evolving an engram
// that has ALREADY been evolved must be refused, and the error must name the
// successor to evolve instead.
//
// THE DEFECT (observed live by the scalping desk 2026-07-29): a soft-deleted engram
// keeps resolving, so a second Evolve against the same predecessor id succeeded and
// minted a SECOND successor. Both successors were active, both ranked in recall, and
// nothing in either response hinted at the duplicate. The trigger is a STALE id --
// any caller holding an id that went stale the moment it was first evolved: a repair
// re-run, a retried tool call, an id copied into a note or a boot anchor.
func TestEvolve_RefusesAlreadySupersededEngram(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	ws := eng.store.ResolveVaultPrefix("test")

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Original", Content: "v1 content",
	})
	require.NoError(t, err)
	oldULID, err := storage.ParseULID(resp.ID)
	require.NoError(t, err)

	// First evolve off the live head: must succeed.
	firstID, err := eng.Evolve(ctx, "test", resp.ID, "v2 content", "first revision", nil)
	require.NoError(t, err, "the first evolve off a live engram must succeed")

	// POWER CHECK: the predecessor really is superseded now, or the refusal below
	// could pass for the wrong reason (e.g. a guard that refuses everything).
	oldEng, err := eng.store.GetEngram(ctx, ws, oldULID)
	require.NoError(t, err)
	require.NotNil(t, oldEng, "predecessor must still RESOLVE -- that is what makes the fork reachable")
	require.Equal(t, storage.StateSoftDeleted, oldEng.State, "precondition: predecessor soft-deleted")

	// THE GUARD: a second evolve against the SAME (now stale) predecessor id.
	forkID, err := eng.Evolve(ctx, "test", resp.ID, "v2-prime content", "accidental re-run", nil)
	require.Error(t, err, "evolving an already-superseded engram must be REFUSED, not forked")
	assert.ErrorIs(t, err, ErrAlreadySuperseded)
	assert.Equal(t, storage.ULID{}, forkID, "a refused evolve must not return a new id")
	assert.Contains(t, err.Error(), firstID.String(),
		"the error must NAME the successor so the caller knows which id to evolve instead")

	// AND NO FORK WAS CREATED: exactly one RelSupersedes edge targets the
	// predecessor, so the version chain still has a single head.
	rev, err := eng.store.GetReverseAssociations(ctx, ws, oldULID, 64)
	require.NoError(t, err)
	successors := 0
	for _, a := range rev {
		if a.RelType == storage.RelSupersedes {
			successors++
		}
	}
	assert.Equal(t, 1, successors,
		"the predecessor must have exactly ONE successor; two means the fork was written anyway")
}

// TestEvolve_HeadChainingStillAllowed pins the SAFE side of the fork guard's
// boundary: evolving the CURRENT head repeatedly is legitimate and must keep
// working. The guard discriminates on "is this engram already superseded", not on
// "has this chain been evolved before", so a walk down the chain is unaffected.
//
// This half is the one worth pinning. It was asserted as safe from inference for a
// while before anyone exercised it, and an unexercised safe case is the dangerous
// direction -- it invites exactly the call the rule forbids. The vor desk gave it a
// behavioural receipt against live prod on 2026-07-29 (two evolves each off the
// then-current head: both predecessors soft-deleted with correct forward pointers,
// exactly one record active, one hit in recall). This test is that receipt, pinned,
// so it can never quietly decay back into an inference.
func TestEvolve_HeadChainingStillAllowed(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	ws := eng.store.ResolveVaultPrefix("test")

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Original", Content: "v1 content",
	})
	require.NoError(t, err)

	// v1 -> v2, off the live head.
	secondID, err := eng.Evolve(ctx, "test", resp.ID, "v2 content", "first revision", nil)
	require.NoError(t, err)

	// v2 -> v3, off the NEW head. This is the case the guard must NOT block.
	thirdID, err := eng.Evolve(ctx, "test", secondID.String(), "v3 content", "second revision", nil)
	require.NoError(t, err, "chaining off the CURRENT head must remain allowed")
	require.NotEqual(t, secondID, thirdID)

	// Exactly one live record at the end of the chain: v3 active, v2 superseded.
	thirdEng, err := eng.store.GetEngram(ctx, ws, thirdID)
	require.NoError(t, err)
	require.NotNil(t, thirdEng)
	assert.Equal(t, storage.StateActive, thirdEng.State, "the chain head must be active")

	secondEng, err := eng.store.GetEngram(ctx, ws, secondID)
	require.NoError(t, err)
	require.NotNil(t, secondEng)
	assert.Equal(t, storage.StateSoftDeleted, secondEng.State,
		"the intermediate version must be soft-deleted, not left live alongside the head")

	// And the chain is walkable one hop at a time: v3 supersedes v2 (not v1).
	rev, err := eng.store.GetReverseAssociations(ctx, ws, secondID, 64)
	require.NoError(t, err)
	var namedSuccessor string
	for _, a := range rev {
		if a.RelType == storage.RelSupersedes {
			namedSuccessor = a.TargetID.String()
		}
	}
	assert.Equal(t, thirdID.String(), namedSuccessor,
		"v2's recorded successor must be v3 -- the immediate-predecessor link a reader walks")
}
