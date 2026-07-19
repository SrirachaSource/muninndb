package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Muninn's reservoir audit (2026-07-19): an engram was read through the
// consumer path, then recalled live at rank one, and its access count never
// moved -- last_access equalled created_at to the nanosecond. Scoring
// feedback fired; the ROW never did. Any retention rule keyed on
// "never accessed" would have swept living memories. These tests read and
// recall through the real consumer surfaces and assert the ROW moves.

func waitForAccess(t *testing.T, eng *Engine, vault string, id storage.ULID, wantAtLeast uint32) *storage.Engram {
	t.Helper()
	ws := eng.store.ResolveVaultPrefix(vault)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		row, err := eng.store.GetEngram(context.Background(), ws, id)
		if err == nil && row != nil && row.AccessCount >= wantAtLeast {
			return row
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, _ := eng.store.GetEngram(context.Background(), ws, id)
	got := uint32(0)
	if row != nil {
		got = row.AccessCount
	}
	t.Fatalf("access count never reached %d (got %d) -- the row was not persisted", wantAtLeast, got)
	return nil
}

func TestRead_PersistsAccessFieldsOnRow(t *testing.T) {
	eng, _, cleanup := testEnvWithHNSW(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "access-test",
		Concept: "read specimen",
		Content: "reading this through the consumer path must move the row's access fields",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, _ := storage.ParseULID(resp.ID)

	if _, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "access-test", ID: resp.ID}); err != nil {
		t.Fatalf("Read: %v", err)
	}

	row := waitForAccess(t, eng, "access-test", id, 1)
	if !row.LastAccess.After(row.CreatedAt) {
		t.Errorf("last_access (%v) not after created_at (%v) -- the muninn signature",
			row.LastAccess, row.CreatedAt)
	}
}

func TestActivate_PersistsAccessOnReturnedHits(t *testing.T) {
	eng, _, cleanup := testEnvWithHNSW(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "access-test",
		Concept:   "recall specimen",
		Content:   "recalling this at rank one must move the row's access fields too",
		Embedding: testVec(8, 0.7),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, _ := storage.ParseULID(resp.ID)

	act, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "access-test",
		Context:    []string{"recall specimen rank one"},
		Embedding:  testVec(8, 0.7),
		MaxResults: 5,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	found := false
	for _, a := range act.Activations {
		if a.ID == resp.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("specimen not in recall results; cannot assert access persistence")
	}

	waitForAccess(t, eng, "access-test", id, 1)
}
