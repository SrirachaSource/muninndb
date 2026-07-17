package storage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// vaultScopedExportPrefixes lists every prefix scoped by a vault workspace
// that should be exported. 0x01 (engrams) is iterated first with the engram
// count tracked. 0x0E (VaultMetaKey) and 0x0F (VaultNameIndexKey) are
// global/name keys excluded from the data stream (written by WriteVaultName
// on import). 0x11 (DigestFlagsKey) is globally keyed by ULID — excluded.
var vaultScopedExportPrefixes = []byte{
	0x01, // engrams (full record)
	0x02, // metadata-only
	0x03, // forward associations
	0x04, // reverse associations
	0x05, // FTS posting lists
	0x06, // trigrams
	0x07, // HNSW node neighbors
	0x08, // FTS global stats
	0x09, // per-term FTS stats
	0x0A, // contradictions
	0x0B, // state secondary index
	0x0C, // tag secondary index
	0x0D, // creator secondary index
	0x10, // relevance bucket index
	0x12, // coherence counters
	0x13, // vault scoring weights
	0x14, // association weight index
	0x15, // vault count key
	0x16, // provenance
	0x17, // bucket migration state
	0x18, // standalone embeddings (ERF v2 EmbeddingKey)
	0x1A, // episode keys (EpisodeKey + EpisodeFrameKey)
	0x1B, // FTS schema version marker
	0x20, // engram→entity forward links (value = entity name)
	0x21, // entity-to-entity relationship records
	0x24, // entity co-occurrence counts
	0x26, // relationship entity index
	0x28, // content-hash dedup index
	// 0x23 (entity→engram reverse index) is NOT exported: its ws sits at bytes
	// 9-16 (after the name hash), which the strip/reinsert format can't carry.
	// Import rebuilds it from each 0x20 key instead. 0x1F (global entity
	// records) is vault-agnostic and likewise excluded; import re-seeds a
	// minimal record per imported entity name so lookups resolve.
}

const exportBatchSize = 512

// ExportVaultData streams all vault-scoped keys to w as a gzip'd tar archive.
//
// The archive contains three entries:
//   - manifest.json: JSON-encoded MuninnManifest
//   - data.kvs: binary stream of (key_len uint32 BE)(key bytes)(val_len uint32 BE)(val bytes)
//     where the 8 workspace bytes are stripped from bytes 1-8 of every key.
//   - checksum.txt: SHA256 checksum of the data.kvs byte stream ("sha256:<hex>\n")
//
// Returns an ExportResult with the engram count and total key count.
func (ps *PebbleStore) ExportVaultData(
	ctx context.Context,
	ws [8]byte,
	vaultName string,
	opts ExportOpts,
	w io.Writer,
) (*ExportResult, error) {
	wsNext, err := incrementWS(ws)
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}

	// Acquire a snapshot so both passes see a consistent view of the data.
	snap := ps.db.NewSnapshot()
	defer snap.Close()

	// Pass 1: iterate over snapshot to count total byte size of the KV stream
	// and count engrams/total keys. No data is stored.
	var kvSize int64
	var engramCount int64
	var totalKeys int64

	for _, p := range vaultScopedExportPrefixes {
		lo := make([]byte, 9)
		lo[0] = p
		copy(lo[1:], ws[:])
		hi := make([]byte, 9)
		hi[0] = p
		copy(hi[1:], wsNext[:])

		iter, iterErr := snap.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if iterErr != nil {
			return nil, fmt.Errorf("export: pass1 iter prefix 0x%02X: %w", p, iterErr)
		}

		for valid := iter.First(); valid; valid = iter.Next() {
			select {
			case <-ctx.Done():
				iter.Close()
				return nil, ctx.Err()
			default:
			}

			k := iter.Key()
			v := iter.Value()

			// stripped key: [prefix_byte][rest of key after ws bytes]
			strippedLen := 1 + len(k) - 9
			// byte size: 4 (key_len) + strippedLen + 4 (val_len) + len(v)
			kvSize += int64(4 + strippedLen + 4 + len(v))

			if p == 0x01 {
				engramCount++
			}
			totalKeys++
		}
		if err := iter.Close(); err != nil {
			return nil, fmt.Errorf("export: pass1 close iter prefix 0x%02X: %w", p, err)
		}
	}

	// Build manifest now that we have the engram count.
	manifest := MuninnManifest{
		MuninnVersion: "1",
		SchemaVersion: MuninnSchemaVersion,
		Vault:         vaultName,
		EmbedderModel: opts.EmbedderModel,
		Dimension:     opts.Dimension,
		EngramCount:   engramCount,
		CreatedAt:     time.Now().UTC(),
		ResetMetadata: opts.ResetMetadata,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("export: marshal manifest: %w", err)
	}

	// Open gzip + tar writers.
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	// Write manifest.json entry.
	if err := tw.WriteHeader(&tar.Header{
		Name:     "manifest.json",
		Mode:     0644,
		Size:     int64(len(manifestBytes)),
		ModTime:  manifest.CreatedAt,
		Typeflag: tar.TypeReg,
	}); err != nil {
		return nil, fmt.Errorf("export: tar header manifest: %w", err)
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		return nil, fmt.Errorf("export: tar write manifest: %w", err)
	}

	// Write data.kvs tar header with the exact size computed in Pass 1.
	if err := tw.WriteHeader(&tar.Header{
		Name:     "data.kvs",
		Mode:     0644,
		Size:     kvSize,
		ModTime:  manifest.CreatedAt,
		Typeflag: tar.TypeReg,
	}); err != nil {
		return nil, fmt.Errorf("export: tar header data.kvs: %w", err)
	}

	// Pass 2: iterate over the same snapshot again, writing each KV entry
	// directly to the tar writer (no in-memory buffer).
	// Simultaneously compute SHA256 of the KV stream via io.MultiWriter.
	h := sha256.New()
	mw := io.MultiWriter(tw, h)
	var lenBuf [4]byte
	for _, p := range vaultScopedExportPrefixes {
		lo := make([]byte, 9)
		lo[0] = p
		copy(lo[1:], ws[:])
		hi := make([]byte, 9)
		hi[0] = p
		copy(hi[1:], wsNext[:])

		iter, iterErr := snap.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if iterErr != nil {
			return nil, fmt.Errorf("export: pass2 iter prefix 0x%02X: %w", p, iterErr)
		}

		for valid := iter.First(); valid; valid = iter.Next() {
			select {
			case <-ctx.Done():
				iter.Close()
				return nil, ctx.Err()
			default:
			}

			k := iter.Key()
			v := iter.Value()

			// Strip the 8 workspace bytes (positions 1-8) from the key.
			// Exported key: [prefix_byte][rest of key after ws bytes]
			stripped := make([]byte, 1+len(k)-9)
			stripped[0] = k[0]
			copy(stripped[1:], k[9:])

			rawVal := make([]byte, len(v))
			copy(rawVal, v)

			// Write (key_len uint32)(key)(val_len uint32)(val) through mw
			// (writes to both tar and the SHA256 hasher simultaneously).
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(stripped)))
			if _, err := mw.Write(lenBuf[:]); err != nil {
				iter.Close()
				return nil, fmt.Errorf("export: write key len: %w", err)
			}
			if _, err := mw.Write(stripped); err != nil {
				iter.Close()
				return nil, fmt.Errorf("export: write key: %w", err)
			}
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(rawVal)))
			if _, err := mw.Write(lenBuf[:]); err != nil {
				iter.Close()
				return nil, fmt.Errorf("export: write val len: %w", err)
			}
			if _, err := mw.Write(rawVal); err != nil {
				iter.Close()
				return nil, fmt.Errorf("export: write val: %w", err)
			}
		}
		if err := iter.Close(); err != nil {
			return nil, fmt.Errorf("export: pass2 close iter prefix 0x%02X: %w", p, err)
		}
	}

	// Append checksum.txt as the last tar entry with the SHA256 of the KV stream.
	checksumContent := []byte("sha256:" + hex.EncodeToString(h.Sum(nil)) + "\n")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "checksum.txt",
		Mode:     0644,
		Size:     int64(len(checksumContent)),
		ModTime:  time.Now(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return nil, fmt.Errorf("export: tar header checksum.txt: %w", err)
	}
	if _, err := tw.Write(checksumContent); err != nil {
		return nil, fmt.Errorf("export: write checksum.txt: %w", err)
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("export: tar close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("export: gzip close: %w", err)
	}

	return &ExportResult{
		EngramCount: engramCount,
		TotalKeys:   totalKeys,
	}, nil
}

// ImportVaultData reads a .muninn gzip'd tar archive from r and writes all
// keys into wsTarget. The 8 workspace bytes stripped during export are
// re-inserted at positions 1-8 of each key using the target ws.
//
// ImportVaultData validates the manifest schema version and (optionally) the
// embedder model/dimension. It does NOT write the vault name index —
// the caller is responsible for calling WriteVaultName before importing.
//
// If checksum.txt is present in the archive, the SHA256 of data.kvs is verified
// before committing the batch. If checksum.txt is absent (legacy export), a
// warning is logged but the import proceeds (backward compatibility).
func (ps *PebbleStore) ImportVaultData(
	ctx context.Context,
	wsTarget [8]byte,
	vaultName string,
	opts ImportOpts,
	r io.Reader,
) (*ExportResult, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("import: gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	var manifest MuninnManifest
	gotManifest := false
	gotKV := false

	// kvResult holds the result accumulated during data.kvs processing.
	// It is populated when we encounter the data.kvs entry and referenced
	// after the loop completes to handle checksum verification and final commit.
	type kvState struct {
		engramCount int64
		totalKeys   int64
		batch       *pebble.Batch
		batchCount  int
		h           interface{ Sum([]byte) []byte } // sha256 hash
		committed   bool
	}
	var kv *kvState

	// Entity names seen on imported 0x20 links; used to re-seed minimal global
	// 0x1F entity records after commit (0x1F is vault-agnostic, not exported).
	importedEntityNames := make(map[string]struct{})

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if kv != nil && kv.batch != nil && !kv.committed {
				kv.batch.Close()
			}
			return nil, fmt.Errorf("import: tar next: %w", err)
		}
		switch hdr.Name {
		case "manifest.json":
			if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
				return nil, fmt.Errorf("import: decode manifest: %w", err)
			}
			gotManifest = true
		case "data.kvs":
			gotKV = true

			if !gotManifest {
				return nil, fmt.Errorf("import: data.kvs encountered before manifest.json")
			}

			if manifest.SchemaVersion != MuninnSchemaVersion && !opts.SkipCompatCheck {
				return nil, fmt.Errorf("import: schema version mismatch: archive=%d, current=%d",
					manifest.SchemaVersion, MuninnSchemaVersion)
			}
			if !opts.SkipCompatCheck && opts.ExpectedModel != "" && manifest.EmbedderModel != opts.ExpectedModel {
				return nil, fmt.Errorf("import: embedder model mismatch: archive=%q, expected=%q",
					manifest.EmbedderModel, opts.ExpectedModel)
			}
			if !opts.SkipCompatCheck && opts.ExpectedDimension != 0 && manifest.Dimension != opts.ExpectedDimension {
				return nil, fmt.Errorf("import: dimension mismatch: archive=%d, expected=%d",
					manifest.Dimension, opts.ExpectedDimension)
			}

			// Replay the KV stream into pebble directly from tr (no io.ReadAll).
			// Compute SHA256 of the stream while reading for later checksum verification.
			h := sha256.New()
			teeR := io.TeeReader(tr, h)

			var engramCount int64
			var totalKeys int64

			// skipIDs tracks engram IDs that already exist in the target vault.
			// All keys associated with a skipped engram ID are omitted from the import.
			skipIDs := make(map[[16]byte]struct{})

			batch := ps.db.NewBatch()
			batchCount := 0

			var lenBuf [4]byte

			for {
				select {
				case <-ctx.Done():
					batch.Close()
					return nil, ctx.Err()
				default:
				}

				// Read key length (through tee so hash covers the full stream).
				if _, err := io.ReadFull(teeR, lenBuf[:]); err == io.EOF || err == io.ErrUnexpectedEOF {
					break
				} else if err != nil {
					batch.Close()
					return nil, fmt.Errorf("import: read key len: %w", err)
				}
				keyLen := binary.BigEndian.Uint32(lenBuf[:])

				strippedKey := make([]byte, keyLen)
				if _, err := io.ReadFull(teeR, strippedKey); err != nil {
					batch.Close()
					return nil, fmt.Errorf("import: read key: %w", err)
				}

				// Read value length.
				if _, err := io.ReadFull(teeR, lenBuf[:]); err != nil {
					batch.Close()
					return nil, fmt.Errorf("import: read val len: %w", err)
				}
				valLen := binary.BigEndian.Uint32(lenBuf[:])

				val := make([]byte, valLen)
				if _, err := io.ReadFull(teeR, val); err != nil {
					batch.Close()
					return nil, fmt.Errorf("import: read val: %w", err)
				}

				// Reconstruct full key: insert the 8 ws bytes at positions 1-8.
				// strippedKey = [prefix_byte][rest...], len >= 1
				fullKey := make([]byte, 1+8+len(strippedKey)-1)
				fullKey[0] = strippedKey[0]
				copy(fullKey[1:9], wsTarget[:])
				copy(fullKey[9:], strippedKey[1:])

				prefix := strippedKey[0]

				// Deduplication: for EngramKey (0x01), check if this engram already exists
				// in the target vault. If it does, record its ID in skipIDs so that all
				// related keys are also skipped.
				if prefix == 0x01 {
					if len(strippedKey) >= 17 {
						existing, closer, getErr := ps.db.Get(fullKey)
						if getErr == nil {
							// Engram already exists — skip this ID and all related keys.
							closer.Close()
							_ = existing
							var skipID [16]byte
							copy(skipID[:], strippedKey[1:17])
							skipIDs[skipID] = struct{}{}
							continue // do not write; do not increment counts
						}
						if getErr != pebble.ErrNotFound {
							batch.Close()
							return nil, fmt.Errorf("import: check engram existence: %w", getErr)
						}
					}
				}

				// Skip keys whose engram ID is in skipIDs.
				// Each key type has the engram ID at a specific offset in the stripped key.
				if len(skipIDs) > 0 {
					switch prefix {
					case 0x02, 0x07, 0x16, 0x18, 0x20, 0x21:
						// MetaKey, HNSWNodeKey, ProvenanceKey, EmbeddingKey,
						// EntityEngramLinkKey, EntityRelationshipKey:
						// stripped layout: [prefix(1)][id(16)]...  — ID at bytes 1:17
						if len(strippedKey) >= 17 {
							var id [16]byte
							copy(id[:], strippedKey[1:17])
							if _, skip := skipIDs[id]; skip {
								continue
							}
						}
					case 0x03:
						// AssocFwdKey: stripped = [0x03][src(16)][weight(4)][dst(16)]
						// src ID at bytes 1:17
						if len(strippedKey) >= 17 {
							var id [16]byte
							copy(id[:], strippedKey[1:17])
							if _, skip := skipIDs[id]; skip {
								continue
							}
						}
					case 0x04:
						// AssocRevKey: stripped = [0x04][dst(16)][weight(4)][src(16)]
						// src ID at bytes 21:37
						if len(strippedKey) >= 37 {
							var id [16]byte
							copy(id[:], strippedKey[21:37])
							if _, skip := skipIDs[id]; skip {
								continue
							}
						}
					case 0x10:
						// RelevanceBucketKey: stripped = [0x10][bucket(1)][id(16)]
						// ID at bytes 2:18
						if len(strippedKey) >= 18 {
							var id [16]byte
							copy(id[:], strippedKey[2:18])
							if _, skip := skipIDs[id]; skip {
								continue
							}
						}
					}
				}

				batch.Set(fullKey, val, nil)
				if strippedKey[0] == 0x01 {
					engramCount++
				}
				// Entity forward link: rebuild the 0x23 reverse index entry
				// (not exportable — its ws sits after the name hash) and note
				// the entity name for post-import 0x1F record seeding.
				if prefix == 0x20 && len(strippedKey) >= 25 {
					var engID [16]byte
					copy(engID[:], strippedKey[1:17])
					var nameHash [8]byte
					copy(nameHash[:], strippedKey[17:25])
					batch.Set(keys.EntityReverseIndexKey(nameHash, wsTarget, engID), nil, nil)
					importedEntityNames[string(val)] = struct{}{}
				}
				totalKeys++
				batchCount++

				if batchCount >= exportBatchSize {
					if err := batch.Commit(pebble.NoSync); err != nil {
						batch.Close()
						return nil, fmt.Errorf("import: commit batch: %w", err)
					}
					batch.Close()
					batch = ps.db.NewBatch()
					batchCount = 0
				}
			}

			// Hold the final batch uncommitted — defer commit until checksum passes
			// (or until we confirm there's no checksum entry for backward compat).
			kv = &kvState{
				engramCount: engramCount,
				totalKeys:   totalKeys,
				batch:       batch,
				batchCount:  batchCount,
				h:           h,
			}

		case "checksum.txt":
			// Verify SHA256 of data.kvs against the stored checksum.
			if kv == nil {
				// checksum.txt before data.kvs — skip (unexpected ordering).
				continue
			}
			checksumData, readErr := io.ReadAll(tr)
			if readErr != nil {
				kv.batch.Close()
				return nil, fmt.Errorf("import: read checksum.txt: %w", readErr)
			}
			expectedChecksum := strings.TrimSpace(string(checksumData))
			actualChecksum := "sha256:" + hex.EncodeToString(kv.h.Sum(nil))
			if expectedChecksum != actualChecksum {
				kv.batch.Close()
				return nil, fmt.Errorf("import: archive checksum mismatch: expected %s, got %s",
					expectedChecksum, actualChecksum)
			}
			// Checksum verified — commit the final batch now.
			if kv.batchCount > 0 {
				if err := kv.batch.Commit(pebble.Sync); err != nil {
					kv.batch.Close()
					return nil, fmt.Errorf("import: commit final batch (after checksum): %w", err)
				}
			}
			kv.batch.Close()
			kv.committed = true

			// Seed the in-memory vault counter.
			vc := ps.getOrInitCounter(ctx, wsTarget)
			vc.count.Store(kv.engramCount)

			ps.seedImportedEntityRecords(ctx, importedEntityNames)
		}
	}

	// Post-loop: handle the case where data.kvs was processed but checksum.txt
	// was absent (legacy export — backward compatible).
	if kv != nil && !kv.committed {
		slog.Warn("import: archive has no checksum.txt — skipping integrity check (legacy export)",
			"vault", vaultName)
		if kv.batchCount > 0 {
			if err := kv.batch.Commit(pebble.NoSync); err != nil {
				kv.batch.Close()
				return nil, fmt.Errorf("import: commit final batch: %w", err)
			}
		}
		kv.batch.Close()

		// Seed the in-memory vault counter.
		vc := ps.getOrInitCounter(ctx, wsTarget)
		vc.count.Store(kv.engramCount)

		ps.seedImportedEntityRecords(ctx, importedEntityNames)

		return &ExportResult{
			EngramCount: kv.engramCount,
			TotalKeys:   kv.totalKeys,
		}, nil
	}

	// Return result if data.kvs was committed (with checksum verified).
	if kv != nil && kv.committed {
		return &ExportResult{
			EngramCount: kv.engramCount,
			TotalKeys:   kv.totalKeys,
		}, nil
	}

	if !gotManifest {
		return nil, fmt.Errorf("import: archive missing manifest.json")
	}
	if !gotKV {
		// Empty vault — no data.kvs is acceptable if engramCount == 0.
		if manifest.EngramCount != 0 {
			return nil, fmt.Errorf("import: archive missing data.kvs but manifest shows %d engrams", manifest.EngramCount)
		}

		if manifest.SchemaVersion != MuninnSchemaVersion && !opts.SkipCompatCheck {
			return nil, fmt.Errorf("import: schema version mismatch: archive=%d, current=%d",
				manifest.SchemaVersion, MuninnSchemaVersion)
		}
		if !opts.SkipCompatCheck && opts.ExpectedModel != "" && manifest.EmbedderModel != opts.ExpectedModel {
			return nil, fmt.Errorf("import: embedder model mismatch: archive=%q, expected=%q",
				manifest.EmbedderModel, opts.ExpectedModel)
		}
		if !opts.SkipCompatCheck && opts.ExpectedDimension != 0 && manifest.Dimension != opts.ExpectedDimension {
			return nil, fmt.Errorf("import: dimension mismatch: archive=%d, expected=%d",
				manifest.Dimension, opts.ExpectedDimension)
		}

		return &ExportResult{EngramCount: 0, TotalKeys: 0}, nil
	}

	// Should not be reached.
	return &ExportResult{EngramCount: 0, TotalKeys: 0}, nil
}

// seedImportedEntityRecords writes a minimal global 0x1F entity record for any
// imported entity name that has none. 0x1F records are vault-agnostic and are
// not part of a vault export; without this, imported 0x20/0x21 links would
// reference entities that resolve to nothing. Existing records are never
// overwritten (UpsertEntityRecord preserves higher-confidence data), and the
// low confidence + "import" source mark these as reconstructable stubs that
// enrichment can later refine.
func (ps *PebbleStore) seedImportedEntityRecords(ctx context.Context, names map[string]struct{}) {
	for name := range names {
		if name == "" {
			continue
		}
		rec, err := ps.GetEntityRecord(ctx, name)
		if err == nil && rec != nil {
			continue
		}
		if err := ps.UpsertEntityRecord(ctx, EntityRecord{
			Name:       name,
			Confidence: 0.5,
			Source:     "import",
			State:      "active",
		}, "import"); err != nil {
			slog.Warn("import: seed entity record failed", "entity", name, "error", err)
		}
	}
}
