package storage

import (
	"context"
	"math"
	"sort"
	"testing"
)

func almostEq(a, b float32) bool {
	return math.Abs(float64(a)-float64(b)) < 1e-6
}

// TestRestoreAssocWeightsToPeak_RaisesDecayedEdges verifies the core recovery:
// an edge ground down by decay (weight << peak) is raised to peak*scale, while
// an edge already at/above its target is left untouched.
func TestRestoreAssocWeightsToPeak_RaisesDecayedEdges(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("restore-raise")

	a, b := NewULID(), NewULID()
	c, d := NewULID(), NewULID()

	// A→B: written at 0.8, then decayed to 0.04 (peak stays 0.8).
	if err := store.WriteAssociation(ctx, ws, a, b, &Association{TargetID: b, Weight: 0.8, RelType: RelSupports}); err != nil {
		t.Fatalf("WriteAssociation A→B: %v", err)
	}
	if err := store.UpdateAssocWeight(ctx, ws, a, b, 0.04, 0); err != nil {
		t.Fatalf("UpdateAssocWeight A→B: %v", err)
	}
	// C→D: healthy at 0.5 (peak 0.5; target 0.125 — must be skipped).
	if err := store.WriteAssociation(ctx, ws, c, d, &Association{TargetID: d, Weight: 0.5, RelType: RelSupports}); err != nil {
		t.Fatalf("WriteAssociation C→D: %v", err)
	}

	counts, err := store.RestoreAssocWeightsToPeak(ctx, ws, 0.25, false, nil)
	if err != nil {
		t.Fatalf("RestoreAssocWeightsToPeak: %v", err)
	}
	if counts.Scanned != 2 || counts.Raised != 1 || counts.Skipped != 1 {
		t.Fatalf("counts = %+v, want Scanned=2 Raised=1 Skipped=1", counts)
	}

	w, err := store.GetAssocWeight(ctx, ws, a, b)
	if err != nil {
		t.Fatalf("GetAssocWeight A→B: %v", err)
	}
	if !almostEq(w, 0.8*0.25) {
		t.Fatalf("A→B weight = %v, want %v", w, 0.8*0.25)
	}
	// Peak preserved, exactly one edge (old key deleted).
	assocs, err := store.GetAssociations(ctx, ws, []ULID{a}, 0)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if n := len(assocs[a]); n != 1 {
		t.Fatalf("A has %d edges, want 1 (old-weight key must be deleted)", n)
	}
	if got := assocs[a][0].PeakWeight; !almostEq(got, 0.8) {
		t.Fatalf("A→B peak = %v, want 0.8", got)
	}
	// Clause 3: the raised edge is stamped as a restore, so the engine's
	// re-establishment rules take over from here.
	if assocs[a][0].RestoredAt == 0 {
		t.Fatal("A→B restoredAt = 0, want a restore stamp")
	}

	wCD, err := store.GetAssocWeight(ctx, ws, c, d)
	if err != nil {
		t.Fatalf("GetAssocWeight C→D: %v", err)
	}
	if !almostEq(wCD, 0.5) {
		t.Fatalf("C→D weight = %v, want 0.5 (untouched)", wCD)
	}
	cdAssocs, err := store.GetAssociations(ctx, ws, []ULID{c}, 0)
	if err != nil {
		t.Fatalf("GetAssociations C: %v", err)
	}
	if cdAssocs[c][0].RestoredAt != 0 {
		t.Fatalf("C→D restoredAt = %d, want 0 (skipped edge must not be stamped)", cdAssocs[c][0].RestoredAt)
	}
}

// TestRestoreAssocWeightsToPeak_DryRunWritesNothing verifies the dry-run walk
// returns the same counts but leaves every weight unchanged.
func TestRestoreAssocWeightsToPeak_DryRunWritesNothing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("restore-dry")

	a, b := NewULID(), NewULID()
	if err := store.WriteAssociation(ctx, ws, a, b, &Association{TargetID: b, Weight: 0.6, RelType: RelSupports}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}
	if err := store.UpdateAssocWeight(ctx, ws, a, b, 0.03, 0); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}

	counts, err := store.RestoreAssocWeightsToPeak(ctx, ws, 0.25, true, nil)
	if err != nil {
		t.Fatalf("RestoreAssocWeightsToPeak dry-run: %v", err)
	}
	if counts.Scanned != 1 || counts.Raised != 1 {
		t.Fatalf("counts = %+v, want Scanned=1 Raised=1", counts)
	}
	w, err := store.GetAssocWeight(ctx, ws, a, b)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if !almostEq(w, 0.03) {
		t.Fatalf("weight = %v, want 0.03 (dry-run must not write)", w)
	}
}

// TestRestoreAssocWeightsToPeak_PreservesPeakOrdering locks the property the
// whole recovery rests on: the decay floor kept peak ORDERING, and restore
// turns amplitude back up without reshuffling it.
func TestRestoreAssocWeightsToPeak_PreservesPeakOrdering(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("restore-order")

	src := NewULID()
	peaks := []float32{0.9, 0.6, 0.3}
	targets := make([]ULID, len(peaks))
	for i, p := range peaks {
		targets[i] = NewULID()
		if err := store.WriteAssociation(ctx, ws, src, targets[i], &Association{TargetID: targets[i], Weight: p, RelType: RelSupports}); err != nil {
			t.Fatalf("WriteAssociation %d: %v", i, err)
		}
		// Grind to the 5% floor.
		if err := store.UpdateAssocWeight(ctx, ws, src, targets[i], p*0.05, 0); err != nil {
			t.Fatalf("UpdateAssocWeight %d: %v", i, err)
		}
	}

	if _, err := store.RestoreAssocWeightsToPeak(ctx, ws, 0.25, false, nil); err != nil {
		t.Fatalf("RestoreAssocWeightsToPeak: %v", err)
	}

	assocs, err := store.GetAssociations(ctx, ws, []ULID{src}, 0)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if len(assocs[src]) != len(peaks) {
		t.Fatalf("edge count = %d, want %d", len(assocs[src]), len(peaks))
	}
	byTarget := make(map[ULID]float32)
	for _, a := range assocs[src] {
		byTarget[a.TargetID] = a.Weight
	}
	restored := make([]float32, len(peaks))
	for i := range peaks {
		restored[i] = byTarget[targets[i]]
		if !almostEq(restored[i], peaks[i]*0.25) {
			t.Fatalf("edge %d weight = %v, want %v", i, restored[i], peaks[i]*0.25)
		}
	}
	if !sort.SliceIsSorted(restored, func(i, j int) bool { return restored[i] > restored[j] }) {
		t.Fatalf("restored weights %v lost peak ordering %v", restored, peaks)
	}
}

// TestRestoreAssocWeightsToPeak_InvalidScale rejects out-of-range scales.
func TestRestoreAssocWeightsToPeak_InvalidScale(t *testing.T) {
	store := newTestStore(t)
	ws := store.VaultPrefix("restore-scale")
	for _, s := range []float32{0, -0.5, 1.5} {
		if _, err := store.RestoreAssocWeightsToPeak(context.Background(), ws, s, true, nil); err == nil {
			t.Fatalf("scale %v: expected error, got nil", s)
		}
	}
}
