package storage_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

// The KLAC 2026-07-18 defect: SetDigestFlag (embed processor) and UpdateDigest
// (enrich processor) both read-modify-write the same digest-flags byte with no
// lock, so whichever committed second clobbered the other's bit -- KLAC read
// flag_embedded FALSE over healthy stores. These tests hammer both halves
// concurrently; without digestFlagMu they lose bits, with it they never do.
// Run with -race in CI for the memory-model half of the proof.

func openFlagsDB(t *testing.T) (*storage.PebbleStore, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-flagrace-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{})
	return store, func() {
		store.Close()
		os.RemoveAll(dir)
	}
}

func TestSetDigestFlag_ConcurrentDistinctBitsAllSurvive(t *testing.T) {
	store, cleanup := openFlagsDB(t)
	defer cleanup()
	ctx := context.Background()
	id := storage.NewULID()

	bits := []uint8{0x01, 0x02, 0x04, 0x08, 0x10, 0x20, 0x40, 0x80}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, b := range bits {
		wg.Add(1)
		go func(bit uint8) {
			defer wg.Done()
			<-start
			for i := 0; i < 100; i++ {
				if err := store.SetDigestFlag(ctx, id, bit); err != nil {
					t.Errorf("SetDigestFlag(0x%02x): %v", bit, err)
					return
				}
			}
		}(b)
	}
	close(start)
	wg.Wait()

	got, err := store.GetDigestFlags(ctx, id)
	if err != nil {
		t.Fatalf("GetDigestFlags: %v", err)
	}
	if got != 0xFF {
		t.Errorf("lost update: flags = 0x%02x, want 0xFF (every concurrent bit kept)", got)
	}
}

func TestSetDigestFlag_vs_UpdateDigest_NoLostBit(t *testing.T) {
	// The actual production interleaving: enrich lands a digest (summarized |
	// classified via UpdateDigest's RMW-through-batch-commit) while embed sets
	// its bit via SetDigestFlag. All three bits must survive.
	store, cleanup := openFlagsDB(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("flag-race-vault")

	id, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "flag race specimen",
		Content: "embed and enrich race this engram's digest flags byte",
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 50; i++ {
			if err := store.SetDigestFlag(ctx, id, plugin.DigestEmbed); err != nil {
				t.Errorf("SetDigestFlag: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 50; i++ {
			if err := store.UpdateDigest(ctx, id, "a summary", []string{"kp"},
				"fact", "fact"); err != nil {
				t.Errorf("UpdateDigest: %v", err)
				return
			}
		}
	}()
	close(start)
	wg.Wait()

	got, err := store.GetDigestFlags(ctx, id)
	if err != nil {
		t.Fatalf("GetDigestFlags: %v", err)
	}
	if got&plugin.DigestEmbed == 0 {
		t.Errorf("embed bit LOST (flags=0x%02x) -- the KLAC signature", got)
	}
	if got&0x40 == 0 || got&0x20 == 0 {
		t.Errorf("digest bits lost (flags=0x%02x), want summarized|classified kept", got)
	}
}
