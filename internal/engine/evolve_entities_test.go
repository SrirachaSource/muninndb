package engine

import (
	"context"
	"sort"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func engramEntityNames(t *testing.T, store *storage.PebbleStore, ws [8]byte, id storage.ULID) []string {
	t.Helper()
	var names []string
	if err := store.ScanEngramEntities(context.Background(), ws, id, func(name string) error {
		names = append(names, name)
		return nil
	}); err != nil {
		t.Fatalf("ScanEngramEntities(%s): %v", id.String(), err)
	}
	sort.Strings(names)
	return names
}

func engramRelationships(t *testing.T, store *storage.PebbleStore, ws [8]byte, id storage.ULID) []storage.RelationshipRecord {
	t.Helper()
	var recs []storage.RelationshipRecord
	if err := store.ScanEngramRelationships(context.Background(), ws, id, func(rec storage.RelationshipRecord) error {
		recs = append(recs, rec)
		return nil
	}); err != nil {
		t.Fatalf("ScanEngramRelationships(%s): %v", id.String(), err)
	}
	return recs
}

// TestEvolve_PreservesCallerSetEntities is the regression lock for the
// entity-strip defect: evolving an engram minted a new ULID but left every
// entity link and entity-relationship record bound to the soft-deleted old id,
// so the live version silently lost its place in the entity graph (caller-set
// entities gone, graph edges orphaned).
func TestEvolve_PreservesCallerSetEntities(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "evolve-ent",
		Concept: "INTC sovereign floor",
		Content: "the government stake sets a price floor under INTC",
		Entities: []mbp.InlineEntity{
			{Name: "INTC", Type: "ticker"},
			{Name: "US Government", Type: "org"},
		},
		EntityRelationships: []mbp.InlineEntityRelationship{
			{FromEntity: "US Government", ToEntity: "INTC", RelType: "holds_stake_in", Weight: 0.9},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	oldULID, _ := storage.ParseULID(resp.ID)
	ws := store.ResolveVaultPrefix("evolve-ent")

	wantNames := engramEntityNames(t, store, ws, oldULID)
	if len(wantNames) != 2 {
		t.Fatalf("precondition: old engram has %d entity links, want 2 (%v)", len(wantNames), wantNames)
	}
	wantRels := engramRelationships(t, store, ws, oldULID)
	if len(wantRels) == 0 {
		t.Fatal("precondition: old engram has no relationship records")
	}

	newULID, err := eng.Evolve(ctx, "evolve-ent", resp.ID, "the price floor thesis, reworded", "wording repair", nil)
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}

	gotNames := engramEntityNames(t, store, ws, newULID)
	if len(gotNames) != len(wantNames) {
		t.Fatalf("evolved engram entity links = %v, want %v", gotNames, wantNames)
	}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Fatalf("evolved engram entity links = %v, want %v", gotNames, wantNames)
		}
	}

	gotRels := engramRelationships(t, store, ws, newULID)
	if len(gotRels) != len(wantRels) {
		t.Fatalf("evolved engram has %d relationship records, want %d", len(gotRels), len(wantRels))
	}
	foundInline := false
	for _, r := range gotRels {
		if r.FromEntity == "US Government" && r.ToEntity == "INTC" && r.RelType == "holds_stake_in" {
			foundInline = true
			if r.Weight != 0.9 {
				t.Errorf("migrated relationship weight = %v, want 0.9 (records must copy wholesale)", r.Weight)
			}
		}
	}
	if !foundInline {
		t.Errorf("caller-set relationship missing from evolved engram: %+v", gotRels)
	}

	// The entity digest flags must carry so the enricher does not re-derive
	// over the migrated caller-set entities.
	newFlags, err := store.GetDigestFlags(ctx, newULID)
	if err != nil {
		t.Fatalf("GetDigestFlags(new): %v", err)
	}
	if newFlags&plugin.DigestEntities == 0 {
		t.Errorf("DigestEntities flag not carried to evolved engram (flags=0x%02x)", newFlags)
	}

	// Additive migration: the soft-deleted original keeps its own entity graph
	// so it stays restorable intact.
	oldNames := engramEntityNames(t, store, ws, oldULID)
	if len(oldNames) != 2 {
		t.Errorf("old engram lost its entity links after evolve: %v", oldNames)
	}
}

// TestEvolve_NoEntities_NoFlagCarry: evolving an engram with no entity graph
// stays clean — no links invented, no entity flags set.
func TestEvolve_NoEntities_NoFlagCarry(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "evolve-bare",
		Concept: "bare",
		Content: "an engram with no entities at all",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	newULID, err := eng.Evolve(ctx, "evolve-bare", resp.ID, "still no entities", "wording", nil)
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}

	ws := store.ResolveVaultPrefix("evolve-bare")
	if names := engramEntityNames(t, store, ws, newULID); len(names) != 0 {
		t.Errorf("bare evolve invented entity links: %v", names)
	}
	newFlags, _ := store.GetDigestFlags(ctx, newULID)
	if newFlags&(plugin.DigestEntities|plugin.DigestRelationships) != 0 {
		t.Errorf("bare evolve set entity digest flags (flags=0x%02x)", newFlags)
	}
}
