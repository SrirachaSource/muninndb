package engine

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func weightsClose(a, b float32) bool {
	return math.Abs(float64(a)-float64(b)) < 1e-6
}

// TestReweightLinks_DryRunReportsWithoutWriting: dry-run reports current
// weight/peak/duplicates per pair and leaves storage untouched.
func TestReweightLinks_DryRunReportsWithoutWriting(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	ra, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "a", Content: "src"})
	rb, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "b", Content: "dst"})
	if _, err := eng.Link(ctx, &mbp.LinkRequest{SourceID: ra.ID, TargetID: rb.ID, RelType: 3, Weight: 0.05, Vault: "test"}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	res, err := eng.ReweightLinks(ctx, "test", []ReweightPair{{SourceID: ra.ID, TargetID: rb.ID, Weight: 0.5}}, true)
	if err != nil {
		t.Fatalf("ReweightLinks dry-run: %v", err)
	}
	item := res.Items[0]
	if item.Status != "would_apply" {
		t.Fatalf("status = %q, want would_apply (detail %q)", item.Status, item.Detail)
	}
	if !weightsClose(item.OldWeight, 0.05) || !weightsClose(item.NewWeight, 0.5) {
		t.Fatalf("old/new = %v/%v, want 0.05/0.5", item.OldWeight, item.NewWeight)
	}
	if item.DuplicateKeys != 1 {
		t.Fatalf("duplicate_keys = %d, want 1", item.DuplicateKeys)
	}
	if res.Applied != 0 {
		t.Fatalf("applied = %d, want 0 on dry-run", res.Applied)
	}

	ws := store.ResolveVaultPrefix("test")
	src, _ := storage.ParseULID(ra.ID)
	dst, _ := storage.ParseULID(rb.ID)
	w, _ := store.GetAssocWeight(ctx, ws, src, dst)
	if !weightsClose(w, 0.05) {
		t.Fatalf("stored weight = %v, want 0.05 (dry-run must not write)", w)
	}
}

// TestReweightLinks_ApplyUpdatesEdgeInPlace: the regression lock for the
// weight-in-key trap — after apply, the pair has exactly ONE forward edge at
// the new weight, with relType and createdAt preserved (a re-Link would have
// doubled the edge and reset metadata).
func TestReweightLinks_ApplyUpdatesEdgeInPlace(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	ra, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "a", Content: "src"})
	rb, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "b", Content: "dst"})
	if _, err := eng.Link(ctx, &mbp.LinkRequest{SourceID: ra.ID, TargetID: rb.ID, RelType: 3, Weight: 0.05, Vault: "test"}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	ws := store.ResolveVaultPrefix("test")
	src, _ := storage.ParseULID(ra.ID)
	dst, _ := storage.ParseULID(rb.ID)
	before, err := store.GetAssociations(ctx, ws, []storage.ULID{src}, 0)
	if err != nil {
		t.Fatalf("GetAssociations before: %v", err)
	}
	createdBefore := before[src][0].CreatedAt

	res, err := eng.ReweightLinks(ctx, "test", []ReweightPair{{SourceID: ra.ID, TargetID: rb.ID, Weight: 0.5}}, false)
	if err != nil {
		t.Fatalf("ReweightLinks: %v", err)
	}
	if res.Items[0].Status != "applied" || res.Applied != 1 {
		t.Fatalf("status/applied = %q/%d, want applied/1", res.Items[0].Status, res.Applied)
	}

	after, err := store.GetAssociations(ctx, ws, []storage.ULID{src}, 0)
	if err != nil {
		t.Fatalf("GetAssociations after: %v", err)
	}
	if n := len(after[src]); n != 1 {
		t.Fatalf("edge count = %d, want exactly 1 (no ghost)", n)
	}
	edge := after[src][0]
	if edge.TargetID != dst {
		t.Fatalf("target = %v, want %v", edge.TargetID, dst)
	}
	if !weightsClose(edge.Weight, 0.5) {
		t.Fatalf("weight = %v, want 0.5", edge.Weight)
	}
	if edge.RelType != 3 {
		t.Fatalf("relType = %d, want 3 (metadata must be preserved)", edge.RelType)
	}
	if !edge.CreatedAt.Equal(createdBefore) {
		t.Fatalf("createdAt changed %v -> %v (must be preserved)", createdBefore, edge.CreatedAt)
	}
	if !weightsClose(edge.PeakWeight, 0.5) {
		t.Fatalf("peak = %v, want 0.5 (monotone max)", edge.PeakWeight)
	}
}

// TestReweightLinks_Statuses covers the per-pair failure modes: missing edge,
// bad ULID, out-of-range weight, soft-deleted endpoint.
func TestReweightLinks_Statuses(t *testing.T) {
	eng, _, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	ra, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "a", Content: "src"})
	rb, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "b", Content: "dst"})
	rc, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "c", Content: "gone"})
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{ID: rc.ID, Hard: false, Vault: "test"}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	res, err := eng.ReweightLinks(ctx, "test", []ReweightPair{
		{SourceID: ra.ID, TargetID: rb.ID, Weight: 0.5},        // no edge exists
		{SourceID: "not-a-ulid", TargetID: rb.ID, Weight: 0.5}, // bad id
		{SourceID: ra.ID, TargetID: rb.ID, Weight: 1.5},        // bad weight
		{SourceID: ra.ID, TargetID: rc.ID, Weight: 0.5},        // soft-deleted target
	}, false)
	if err != nil {
		t.Fatalf("ReweightLinks: %v", err)
	}
	want := []string{"edge_not_found", "invalid_id", "invalid_weight", "soft_deleted"}
	for i, w := range want {
		if res.Items[i].Status != w {
			t.Fatalf("item %d status = %q, want %q (detail %q)", i, res.Items[i].Status, w, res.Items[i].Detail)
		}
	}
	if res.Applied != 0 {
		t.Fatalf("applied = %d, want 0", res.Applied)
	}
}

// TestReweightLinks_ReportsGhostDuplicates: a pair that was double-linked at
// two different weights (the muninn_link weight-in-key trap) is reported with
// duplicate_keys=2 so the census can see the ghost.
func TestReweightLinks_ReportsGhostDuplicates(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	ra, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "a", Content: "src"})
	rb, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "b", Content: "dst"})
	if _, err := eng.Link(ctx, &mbp.LinkRequest{SourceID: ra.ID, TargetID: rb.ID, RelType: 3, Weight: 0.05, Vault: "test"}); err != nil {
		t.Fatalf("Link: %v", err)
	}
	// Second write at a different weight = the ghost (what a re-Link does).
	ws := store.ResolveVaultPrefix("test")
	src, _ := storage.ParseULID(ra.ID)
	dst, _ := storage.ParseULID(rb.ID)
	if err := store.WriteAssociation(ctx, ws, src, dst, &storage.Association{TargetID: dst, Weight: 0.8, RelType: 3}); err != nil {
		t.Fatalf("WriteAssociation ghost: %v", err)
	}

	res, err := eng.ReweightLinks(ctx, "test", []ReweightPair{{SourceID: ra.ID, TargetID: rb.ID, Weight: 0.5}}, true)
	if err != nil {
		t.Fatalf("ReweightLinks: %v", err)
	}
	if res.Items[0].DuplicateKeys != 2 {
		t.Fatalf("duplicate_keys = %d, want 2", res.Items[0].DuplicateKeys)
	}
}

// TestRestoreLinkWeights_JobRestoresDecayedEdge: end-to-end mass recovery — a
// decayed edge (weight << peak) comes back to peak*scale via the background job.
func TestRestoreLinkWeights_JobRestoresDecayedEdge(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	ra, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "a", Content: "src"})
	rb, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "b", Content: "dst"})
	if _, err := eng.Link(ctx, &mbp.LinkRequest{SourceID: ra.ID, TargetID: rb.ID, RelType: 3, Weight: 0.8, Vault: "test"}); err != nil {
		t.Fatalf("Link: %v", err)
	}
	ws := store.ResolveVaultPrefix("test")
	src, _ := storage.ParseULID(ra.ID)
	dst, _ := storage.ParseULID(rb.ID)
	// Grind to the floor, peak preserved.
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.04, 0); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}

	// Dry-run first: one raisable edge, nothing written.
	dry, err := eng.RestoreLinkWeightsDryRun(ctx, "test", 0.25)
	if err != nil {
		t.Fatalf("RestoreLinkWeightsDryRun: %v", err)
	}
	if dry.WouldRaise != 1 {
		t.Fatalf("would_raise = %d, want 1", dry.WouldRaise)
	}

	job, err := eng.StartRestoreLinkWeights(ctx, "test", 0.25)
	if err != nil {
		t.Fatalf("StartRestoreLinkWeights: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for job.GetStatus() != vaultjob.StatusDone {
		if time.Now().After(deadline) {
			t.Fatalf("job did not complete: status %v", job.GetStatus())
		}
		time.Sleep(20 * time.Millisecond)
	}

	w, err := store.GetAssocWeight(ctx, ws, src, dst)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if !weightsClose(w, 0.8*0.25) {
		t.Fatalf("restored weight = %v, want %v", w, 0.8*0.25)
	}
}

// TestRestoreLinkWeights_UnknownVault: both restore entry points 404 an
// unregistered vault.
func TestRestoreLinkWeights_UnknownVault(t *testing.T) {
	eng, _, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.RestoreLinkWeightsDryRun(ctx, "no-such-vault", 0.25); !errors.Is(err, ErrVaultNotFound) {
		t.Fatalf("dry-run err = %v, want ErrVaultNotFound", err)
	}
	if _, err := eng.StartRestoreLinkWeights(ctx, "no-such-vault", 0.25); !errors.Is(err, ErrVaultNotFound) {
		t.Fatalf("start err = %v, want ErrVaultNotFound", err)
	}
}
