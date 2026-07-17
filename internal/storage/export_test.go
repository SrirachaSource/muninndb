package storage

import (
	"github.com/scrypster/muninndb/internal/storage/keys"
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestExportImportRoundtrip(t *testing.T) {
	src := openTestStore(t)
	dst := openTestStore(t)

	ctx := context.Background()

	ws := src.VaultPrefix("vault-a")
	if err := src.WriteVaultName(ws, "vault-a"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Write a few engrams.
	for i := 0; i < 3; i++ {
		eng := &Engram{
			Concept: "concept",
			Content: "content body",
			Tags:    []string{"tag1"},
		}
		if _, err := src.WriteEngram(ctx, ws, eng); err != nil {
			t.Fatalf("WriteEngram: %v", err)
		}
	}

	opts := ExportOpts{EmbedderModel: "all-MiniLM-L6-v2", Dimension: 384}

	var buf bytes.Buffer
	result, err := src.ExportVaultData(ctx, ws, "vault-a", opts, &buf)
	if err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}
	if result.EngramCount != 3 {
		t.Errorf("EngramCount: got %d, want 3", result.EngramCount)
	}
	if result.TotalKeys == 0 {
		t.Errorf("TotalKeys: expected > 0")
	}

	// Import into a new vault on the destination store.
	wsB := dst.VaultPrefix("vault-b")
	if err := dst.WriteVaultName(wsB, "vault-b"); err != nil {
		t.Fatalf("dst WriteVaultName: %v", err)
	}
	iOpts := ImportOpts{}
	iResult, err := dst.ImportVaultData(ctx, wsB, "vault-b", iOpts, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportVaultData: %v", err)
	}
	if iResult.EngramCount != 3 {
		t.Errorf("ImportVaultData EngramCount: got %d, want 3", iResult.EngramCount)
	}
}

func TestImportDeduplication(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	ws := store.VaultPrefix("dedup-vault")
	if err := store.WriteVaultName(ws, "dedup-vault"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Write 2 engrams.
	for i := 0; i < 2; i++ {
		eng := &Engram{
			Concept: "concept",
			Content: "content body",
			Tags:    []string{"tag1"},
		}
		if _, err := store.WriteEngram(ctx, ws, eng); err != nil {
			t.Fatalf("WriteEngram: %v", err)
		}
	}

	opts := ExportOpts{EmbedderModel: "all-MiniLM-L6-v2", Dimension: 384}

	var buf bytes.Buffer
	exportResult, err := store.ExportVaultData(ctx, ws, "dedup-vault", opts, &buf)
	if err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}
	if exportResult.EngramCount != 2 {
		t.Errorf("ExportVaultData EngramCount: got %d, want 2", exportResult.EngramCount)
	}

	exportedBytes := buf.Bytes()

	// First import: both engrams are new, count should be 2.
	iOpts := ImportOpts{SkipCompatCheck: true}
	firstResult, err := store.ImportVaultData(ctx, ws, "dedup-vault", iOpts, bytes.NewReader(exportedBytes))
	if err != nil {
		t.Fatalf("first ImportVaultData: %v", err)
	}
	if firstResult.EngramCount != 0 {
		t.Errorf("first import EngramCount: got %d, want 0 (all were already present from WriteEngram)", firstResult.EngramCount)
	}

	// Second import of the same archive: all engrams already exist, count must be 0.
	secondResult, err := store.ImportVaultData(ctx, ws, "dedup-vault", iOpts, bytes.NewReader(exportedBytes))
	if err != nil {
		t.Fatalf("second ImportVaultData: %v", err)
	}
	if secondResult.EngramCount != 0 {
		t.Errorf("second import EngramCount: got %d, want 0 (duplicates should be skipped)", secondResult.EngramCount)
	}
}

func TestExportEmptyVault(t *testing.T) {
	src := openTestStore(t)
	ctx := context.Background()

	ws := src.VaultPrefix("empty-vault")
	if err := src.WriteVaultName(ws, "empty-vault"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	opts := ExportOpts{}
	var buf bytes.Buffer
	result, err := src.ExportVaultData(ctx, ws, "empty-vault", opts, &buf)
	if err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}
	if result.EngramCount != 0 {
		t.Errorf("expected 0 engrams, got %d", result.EngramCount)
	}

	// Should still be importable.
	dst := openTestStore(t)
	wsD := dst.VaultPrefix("dest-empty")
	if err := dst.WriteVaultName(wsD, "dest-empty"); err != nil {
		t.Fatalf("dst WriteVaultName: %v", err)
	}
	iResult, err := dst.ImportVaultData(ctx, wsD, "dest-empty", ImportOpts{}, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportVaultData empty: %v", err)
	}
	if iResult.EngramCount != 0 {
		t.Errorf("imported engram count: got %d, want 0", iResult.EngramCount)
	}
}

// TestImport_CorruptChecksum verifies that importing an archive with a tampered
// checksum returns an error and does not commit any engrams to the store.
func TestImport_CorruptChecksum(t *testing.T) {
	ctx := context.Background()

	// 1. Export 2 engrams from "chk-src".
	src := openTestStore(t)
	wsSrc := src.VaultPrefix("chk-src")
	if err := src.WriteVaultName(wsSrc, "chk-src"); err != nil {
		t.Fatalf("WriteVaultName src: %v", err)
	}
	for i := 0; i < 2; i++ {
		eng := &Engram{Concept: "concept", Content: "body"}
		if _, err := src.WriteEngram(ctx, wsSrc, eng); err != nil {
			t.Fatalf("WriteEngram: %v", err)
		}
	}

	var exported bytes.Buffer
	_, err := src.ExportVaultData(ctx, wsSrc, "chk-src", ExportOpts{}, &exported)
	if err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}

	// 2. Tamper: decompress → re-tar with a bad checksum → recompress.
	tampered, err := tamperChecksum(exported.Bytes())
	if err != nil {
		t.Fatalf("tamperChecksum: %v", err)
	}

	// 3. Import into "chk-dst" — must fail with a checksum error.
	dst := openTestStore(t)
	wsDst := dst.VaultPrefix("chk-dst")
	if err := dst.WriteVaultName(wsDst, "chk-dst"); err != nil {
		t.Fatalf("WriteVaultName dst: %v", err)
	}
	_, importErr := dst.ImportVaultData(ctx, wsDst, "chk-dst", ImportOpts{SkipCompatCheck: true}, bytes.NewReader(tampered))
	if importErr == nil {
		t.Fatal("expected ImportVaultData to return an error for corrupt checksum, got nil")
	}
	if !strings.Contains(importErr.Error(), "checksum") {
		t.Errorf("expected error message to contain 'checksum', got: %v", importErr)
	}

	// 4. Scan "chk-dst" — expect 0 engrams (batch was not committed).
	engrams, err := dst.EngramsByCreatedSince(ctx, wsDst, time.Time{}, 0, 100)
	if err != nil {
		t.Fatalf("EngramsByCreatedSince: %v", err)
	}
	if len(engrams) != 0 {
		t.Errorf("expected 0 engrams in chk-dst after corrupt import, got %d", len(engrams))
	}
}

// tamperChecksum decompresses the gzip+tar archive, replaces checksum.txt with
// a zeroed hash, and returns the re-compressed archive.
func tamperChecksum(archiveData []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archiveData))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	// Collect all entries.
	type entry struct {
		hdr  *tar.Header
		data []byte
	}
	var entries []entry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar next: %w", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read entry %s: %w", hdr.Name, err)
		}
		entries = append(entries, entry{hdr: hdr, data: data})
	}

	// Re-build with a corrupted checksum.txt.
	var out bytes.Buffer
	gzw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzw)
	for _, e := range entries {
		data := e.data
		if e.hdr.Name == "checksum.txt" {
			data = []byte("sha256:0000000000000000000000000000000000000000000000000000000000000000\n")
		}
		hdr := *e.hdr
		hdr.Size = int64(len(data))
		if err := tw.WriteHeader(&hdr); err != nil {
			return nil, fmt.Errorf("write header %s: %w", hdr.Name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("write data %s: %w", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("tar close: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return out.Bytes(), nil
}

// TestImport_LegacyNoChecksum verifies that importing a legacy archive
// (no checksum.txt entry) succeeds and returns nil error.
func TestImport_LegacyNoChecksum(t *testing.T) {
	ctx := context.Background()

	// Build a tar archive manually without a checksum.txt entry.
	manifest := MuninnManifest{
		MuninnVersion: "1",
		SchemaVersion: MuninnSchemaVersion,
		Vault:         "legacy-src",
		EngramCount:   0,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// Write manifest.json.
	if err := tw.WriteHeader(&tar.Header{
		Name:     "manifest.json",
		Mode:     0644,
		Size:     int64(len(manifestBytes)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("manifest header: %v", err)
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		t.Fatalf("manifest write: %v", err)
	}

	// No data.kvs and no checksum.txt — purely empty legacy archive.

	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	// Import into "legacy-dst" — must succeed (nil error).
	dst := openTestStore(t)
	wsDst := dst.VaultPrefix("legacy-dst")
	if err := dst.WriteVaultName(wsDst, "legacy-dst"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	iResult, importErr := dst.ImportVaultData(ctx, wsDst, "legacy-dst", ImportOpts{SkipCompatCheck: true}, bytes.NewReader(buf.Bytes()))
	if importErr != nil {
		t.Fatalf("expected nil error for legacy archive without checksum.txt, got: %v", importErr)
	}
	if iResult.EngramCount != 0 {
		t.Errorf("expected 0 engrams from empty legacy archive, got %d", iResult.EngramCount)
	}
}

// TestExportImportRoundtrip_EntityGraphSurvives is the regression lock for the
// 2026-07-17 finding: vault exports silently omitted the entity graph — 0x20
// forward links and 0x21 relationship records were not in the export prefix
// list, so every R2 backup was missing them and a disaster restore would have
// lost the graph. Export now carries 0x20/0x21; import rebuilds the 0x23
// reverse index from each 0x20 key and seeds minimal global 0x1F records so
// imported entities resolve.
func TestExportImportRoundtrip_EntityGraphSurvives(t *testing.T) {
	src := openTestStore(t)
	dst := openTestStore(t)
	ctx := context.Background()

	ws := src.VaultPrefix("ent-src")
	if err := src.WriteVaultName(ws, "ent-src"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	id, err := src.WriteEngram(ctx, ws, &Engram{
		Concept: "entity carrier",
		Content: "Alice works with Bob.",
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Entity records + engram links + a relationship record on the source.
	for _, name := range []string{"Alice", "Bob"} {
		if err := src.UpsertEntityRecord(ctx, EntityRecord{
			Name: name, Type: "person", Confidence: 0.9, Source: "inline", State: "active",
		}, "inline"); err != nil {
			t.Fatalf("UpsertEntityRecord(%s): %v", name, err)
		}
		if err := src.WriteEntityEngramLink(ctx, ws, id, name); err != nil {
			t.Fatalf("WriteEntityEngramLink(%s): %v", name, err)
		}
	}
	if err := src.UpsertRelationshipRecord(ctx, ws, id, RelationshipRecord{
		FromEntity: "Alice", ToEntity: "Bob", RelType: "works_with",
		Weight: 0.9, Source: "inline",
	}); err != nil {
		t.Fatalf("UpsertRelationshipRecord: %v", err)
	}

	var buf bytes.Buffer
	if _, err := src.ExportVaultData(ctx, ws, "ent-src", ExportOpts{}, &buf); err != nil {
		t.Fatalf("ExportVaultData: %v", err)
	}

	wsB := dst.VaultPrefix("ent-dst")
	if err := dst.WriteVaultName(wsB, "ent-dst"); err != nil {
		t.Fatalf("dst WriteVaultName: %v", err)
	}
	if _, err := dst.ImportVaultData(ctx, wsB, "ent-dst", ImportOpts{}, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("ImportVaultData: %v", err)
	}

	// Forward links (0x20) present on the destination.
	var linked []string
	if err := dst.ScanEngramEntities(ctx, wsB, id, func(name string) error {
		linked = append(linked, name)
		return nil
	}); err != nil {
		t.Fatalf("ScanEngramEntities: %v", err)
	}
	if len(linked) != 2 {
		t.Fatalf("imported engram entity links: got %v, want [Alice Bob]", linked)
	}

	// Relationship records (0x21) present.
	var rels []RelationshipRecord
	if err := dst.ScanEngramRelationships(ctx, wsB, id, func(r RelationshipRecord) error {
		rels = append(rels, r)
		return nil
	}); err != nil {
		t.Fatalf("ScanEngramRelationships: %v", err)
	}
	if len(rels) != 1 || rels[0].RelType != "works_with" {
		t.Fatalf("imported relationships: got %+v, want one works_with", rels)
	}

	// Reverse index (0x23) rebuilt: entity->engram lookup finds the engram.
	for _, name := range []string{"Alice", "Bob"} {
		revKey := keys.EntityReverseIndexKey(keys.EntityNameHash(name), wsB, [16]byte(id))
		_, closer, err := dst.db.Get(revKey)
		if err != nil {
			t.Fatalf("0x23 reverse key missing for %s after import: %v", name, err)
		}
		closer.Close()
	}

	// Global 0x1F records seeded on the destination store.
	for _, name := range []string{"Alice", "Bob"} {
		rec, err := dst.GetEntityRecord(ctx, name)
		if err != nil || rec == nil {
			t.Fatalf("entity record %q not seeded on destination: %v", name, err)
		}
	}
}
