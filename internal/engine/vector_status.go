package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/types"
)

// VectorStatusData is the per-engram embedding/index truth table: every store
// that must agree for semantic search to work, read side by side. It exists
// because the stores CAN disagree (label set, 0x18 row present, HNSW graph
// entry absent) and no other surface exposes the divergence.
type VectorStatusData struct {
	EngramID        string
	Vault           string
	Concept         string
	FlagsRaw        uint8
	FlagEmbedded    bool  // plugin.DigestEmbed set
	FlagFailed0x80  bool  // DigestEmbedFailed/DigestEnrichFailed set (shared bit)
	LabelRaw        uint8 // ERF EmbedDim enum ordinal (NOT a dimension count)
	LabelName       string
	RowPresent      bool // 0x18 quantized embedding row
	RowBytes        int
	VecSlotPresent  bool // 0x07|ws|id|0xFF raw float32 slot
	VecSlotDim      int
	GraphInMemory   bool // node present in the live HNSW graph — the ONLY store Search reads
	GraphTombstoned bool
	GraphEdges      []int // neighbor count per layer (nil when not in graph)
	VaultVectors    int   // total nodes in this vault's graph
	ProbeRequested  bool
	ProbeFound      bool // k=1..K self-search returned this id
	ProbeRank       int  // 1-based rank when found
}

func embedDimName(d types.EmbedDimension) string {
	switch d {
	case types.EmbedNone:
		return "none"
	case types.Embed384:
		return "384"
	case types.Embed768:
		return "768"
	case types.Embed1536:
		return "1536"
	case types.Embed3072:
		return "3072"
	default:
		return "other"
	}
}

// VectorStatus reads every embedding-related store for one engram. Loud on a
// malformed ULID and on a missing engram (parity with Read). Read-only — it
// never repairs, so it is safe to point at a live defect specimen.
func (e *Engine) VectorStatus(ctx context.Context, vault, engramID string, probe bool, probeK int) (*VectorStatusData, error) {
	id, err := storage.ParseULID(engramID)
	if err != nil {
		return nil, fmt.Errorf("vector-status: parse id: %w", err)
	}
	wsPrefix := e.store.ResolveVaultPrefix(vault)

	eng, err := e.store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, ErrEngramNotFound
		}
		return nil, fmt.Errorf("vector-status: get engram: %w", err)
	}

	result := &VectorStatusData{
		EngramID:  id.String(),
		Vault:     vault,
		Concept:   eng.Concept,
		LabelRaw:  uint8(eng.EmbedDim),
		LabelName: embedDimName(types.EmbedDimension(eng.EmbedDim)),
	}

	flags, err := e.store.GetDigestFlags(ctx, id)
	if err != nil && !strings.Contains(err.Error(), "not found") {
		return nil, fmt.Errorf("vector-status: digest flags: %w", err)
	}
	result.FlagsRaw = flags
	result.FlagEmbedded = flags&plugin.DigestEmbed != 0
	result.FlagFailed0x80 = flags&plugin.DigestEmbedFailed != 0

	rowBytes, rowPresent, err := e.store.EmbeddingRowLen(ctx, wsPrefix, id)
	if err != nil {
		return nil, fmt.Errorf("vector-status: embedding row: %w", err)
	}
	result.RowPresent = rowPresent
	result.RowBytes = rowBytes

	if e.hnswRegistry != nil {
		dim, present, err := e.hnswRegistry.StoredVectorDim(wsPrefix, [16]byte(id))
		if err != nil {
			return nil, fmt.Errorf("vector-status: hnsw vector slot: %w", err)
		}
		result.VecSlotPresent = present
		result.VecSlotDim = dim

		inMem, edges, tombstoned := e.hnswRegistry.NodeStatus(wsPrefix, [16]byte(id))
		result.GraphInMemory = inMem
		result.GraphEdges = edges
		result.GraphTombstoned = tombstoned
		result.VaultVectors = e.hnswRegistry.VaultVectors(wsPrefix)

		if probe {
			if probeK <= 0 {
				probeK = 10
			}
			found, rank, err := e.hnswRegistry.SelfProbe(ctx, wsPrefix, [16]byte(id), probeK)
			if err != nil {
				return nil, fmt.Errorf("vector-status: self-probe: %w", err)
			}
			result.ProbeRequested = true
			result.ProbeFound = found
			result.ProbeRank = rank
		}
	}

	return result, nil
}

// VaultVectorAuditData summarizes label/row/graph agreement across a vault and
// samples the IDs of each divergence class.
type VaultVectorAuditData struct {
	Vault         string
	Total         int
	LabelEmbedded int // ERF EmbedDim label != none
	Rows          int // 0x18 rows present
	GraphNodes    int // nodes in the live HNSW graph
	LabelNoRow    []string
	RowNoGraph    []string
	Failed0x80    []string
	SampleCap     int // per-class cap applied to the ID lists above
	Truncated     bool
}

// VaultVectorAudit scans every engram in a vault and cross-checks the three
// stores. Read-only. sampleCap bounds each divergence list (default 50).
func (e *Engine) VaultVectorAudit(ctx context.Context, vault string, sampleCap int) (*VaultVectorAuditData, error) {
	if sampleCap <= 0 {
		sampleCap = 50
	}
	wsPrefix := e.store.ResolveVaultPrefix(vault)
	data := &VaultVectorAuditData{Vault: vault, SampleCap: sampleCap}

	err := e.store.ScanEngrams(ctx, wsPrefix, func(eng *storage.Engram) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		data.Total++
		labeled := types.EmbedDimension(eng.EmbedDim) != types.EmbedNone
		if labeled {
			data.LabelEmbedded++
		}

		_, rowPresent, err := e.store.EmbeddingRowLen(ctx, wsPrefix, eng.ID)
		if err != nil {
			return err
		}
		if rowPresent {
			data.Rows++
		}

		inGraph := false
		if e.hnswRegistry != nil {
			inGraph, _, _ = e.hnswRegistry.NodeStatus(wsPrefix, [16]byte(eng.ID))
			if inGraph {
				data.GraphNodes++
			}
		}

		flags, ferr := e.store.GetDigestFlags(ctx, eng.ID)
		if ferr != nil && !strings.Contains(ferr.Error(), "not found") {
			return ferr
		}

		switch {
		case labeled && !rowPresent:
			if len(data.LabelNoRow) < sampleCap {
				data.LabelNoRow = append(data.LabelNoRow, eng.ID.String())
			} else {
				data.Truncated = true
			}
		case rowPresent && !inGraph:
			if len(data.RowNoGraph) < sampleCap {
				data.RowNoGraph = append(data.RowNoGraph, eng.ID.String())
			} else {
				data.Truncated = true
			}
		}
		if flags&plugin.DigestEmbedFailed != 0 {
			if len(data.Failed0x80) < sampleCap {
				data.Failed0x80 = append(data.Failed0x80, eng.ID.String())
			} else {
				data.Truncated = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("vault vector-audit: %w", err)
	}
	return data, nil
}
