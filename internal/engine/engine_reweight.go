package engine

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/storage"
)

// ReweightPair names one directed association (source -> target) and the weight
// it should carry.
type ReweightPair struct {
	SourceID string  `json:"source_id"`
	TargetID string  `json:"target_id"`
	Weight   float32 `json:"weight"`
}

// ReweightItem is the per-pair outcome of a ReweightLinks call.
type ReweightItem struct {
	SourceID string `json:"source_id"`
	TargetID string `json:"target_id"`
	// Status: applied | would_apply | engram_not_found | edge_not_found |
	// soft_deleted | archived | invalid_id | invalid_weight
	Status            string  `json:"status"`
	Detail            string  `json:"detail,omitempty"`
	OldWeight         float32 `json:"old_weight,omitempty"`
	NewWeight         float32 `json:"new_weight,omitempty"`
	PeakWeight        float32 `json:"peak_weight,omitempty"`
	CoActivationCount uint32  `json:"co_activation_count,omitempty"`
	// DuplicateKeys counts the forward keys present for this pair. A value >1
	// means ghost edges exist from historical double-links: reweight updates
	// only the index-current edge and leaves the ghosts in place (they need a
	// separate prune). 0 means the pair was not inspected (invalid input).
	DuplicateKeys int `json:"duplicate_keys,omitempty"`
}

// ReweightResult summarizes a ReweightLinks call.
type ReweightResult struct {
	Vault     string         `json:"vault"`
	DryRun    bool           `json:"dry_run"`
	Requested int            `json:"requested"`
	Applied   int            `json:"applied"`
	Items     []ReweightItem `json:"items"`
}

// RestoreLinkWeightsResult summarizes a dry-run restore scan.
type RestoreLinkWeightsResult struct {
	Vault      string  `json:"vault"`
	Scale      float32 `json:"scale"`
	DryRun     bool    `json:"dry_run"`
	Scanned    int64   `json:"scanned"`
	WouldRaise int64   `json:"would_raise"`
	Skipped    int64   `json:"skipped"`
}

// MaxReweightPairs bounds a single ReweightLinks call (the REST handler
// rejects larger requests up front with a 400).
const MaxReweightPairs = 10000

const (
	reweightMetaChunk  = 512
	reweightApplyChunk = 1000
)

// ReweightLinks sets the weight of EXISTING associations to caller-supplied
// values (curator repair). Unlike Link, it routes through the storage
// weight-update primitive, so the old-weight key is deleted (no ghost edges)
// and relType/confidence/createdAt/peak/co-activation metadata is preserved.
// Pairs whose edge does not exist are reported edge_not_found and NOT created
// — creating an edge is Link's job, with the metadata semantics that implies.
// dryRun reports per-pair current state (weight, peak, co-activation count,
// duplicate keys) without writing, which doubles as a wire-level census probe
// for known pairs.
func (e *Engine) ReweightLinks(ctx context.Context, vault string, pairs []ReweightPair, dryRun bool) (*ReweightResult, error) {
	if !e.beginVaultOp() {
		return nil, fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	if len(pairs) == 0 {
		return nil, fmt.Errorf("reweight: no pairs supplied")
	}
	if len(pairs) > MaxReweightPairs {
		return nil, fmt.Errorf("reweight: %d pairs exceeds the %d-pair limit", len(pairs), MaxReweightPairs)
	}

	ws := e.store.ResolveVaultPrefix(vault)
	res := &ReweightResult{Vault: vault, DryRun: dryRun, Requested: len(pairs), Items: make([]ReweightItem, len(pairs))}

	// Phase 1: parse + static validation.
	type parsedPair struct {
		idx      int
		src, dst storage.ULID
		weight   float32
	}
	var valid []parsedPair
	for i, p := range pairs {
		item := &res.Items[i]
		item.SourceID, item.TargetID = p.SourceID, p.TargetID
		src, err := storage.ParseULID(p.SourceID)
		if err != nil {
			item.Status = "invalid_id"
			item.Detail = fmt.Sprintf("source_id: %v", err)
			continue
		}
		dst, err := storage.ParseULID(p.TargetID)
		if err != nil {
			item.Status = "invalid_id"
			item.Detail = fmt.Sprintf("target_id: %v", err)
			continue
		}
		if p.Weight <= 0 || p.Weight > 1 {
			item.Status = "invalid_weight"
			item.Detail = "weight must be in (0, 1]"
			continue
		}
		valid = append(valid, parsedPair{idx: i, src: src, dst: dst, weight: p.Weight})
	}

	// Phase 2: endpoint state check (batched metadata reads, mirroring Link's
	// soft-deleted/archived guard).
	states := make(map[storage.ULID]*storage.EngramMeta)
	{
		seen := make(map[storage.ULID]struct{}, len(valid)*2)
		var ids []storage.ULID
		for _, p := range valid {
			for _, id := range [2]storage.ULID{p.src, p.dst} {
				if _, ok := seen[id]; !ok {
					seen[id] = struct{}{}
					ids = append(ids, id)
				}
			}
		}
		for start := 0; start < len(ids); start += reweightMetaChunk {
			end := start + reweightMetaChunk
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]
			metas, err := e.store.GetMetadata(ctx, ws, chunk)
			if err != nil {
				return nil, fmt.Errorf("reweight: check endpoint states: %w", err)
			}
			for i, meta := range metas {
				states[chunk[i]] = meta
			}
		}
	}

	endpointStatus := func(id storage.ULID) (string, bool) {
		meta := states[id]
		switch {
		case meta == nil:
			return "engram_not_found", false
		case meta.State == storage.StateSoftDeleted:
			return "soft_deleted", false
		case meta.State == storage.StateArchived:
			return "archived", false
		}
		return "", true
	}

	// Phase 3: edge inspection — current weight from the O(1) index, peak /
	// co-activation / duplicate count from the per-source association scan.
	type edgeInfo struct {
		peak  float32
		coAct uint32
		dups  int
	}
	edges := make(map[storage.ULID]map[storage.ULID]edgeInfo)
	{
		seen := make(map[storage.ULID]struct{})
		var sources []storage.ULID
		for _, p := range valid {
			if _, ok := seen[p.src]; !ok {
				seen[p.src] = struct{}{}
				sources = append(sources, p.src)
			}
		}
		for start := 0; start < len(sources); start += reweightMetaChunk {
			end := start + reweightMetaChunk
			if end > len(sources) {
				end = len(sources)
			}
			assocMap, err := e.store.GetAssociations(ctx, ws, sources[start:end], 0)
			if err != nil {
				return nil, fmt.Errorf("reweight: read associations: %w", err)
			}
			for src, assocs := range assocMap {
				m := make(map[storage.ULID]edgeInfo)
				for _, a := range assocs {
					info := m[a.TargetID]
					info.dups++
					if a.PeakWeight > info.peak {
						info.peak = a.PeakWeight
					}
					if a.CoActivationCount > info.coAct {
						info.coAct = a.CoActivationCount
					}
					m[a.TargetID] = info
				}
				edges[src] = m
			}
		}
	}

	// Phase 4: fill items; collect applies.
	var updates []storage.AssocWeightUpdate
	var appliedIdx []int
	for _, p := range valid {
		item := &res.Items[p.idx]
		if st, ok := endpointStatus(p.src); !ok {
			item.Status = st
			item.Detail = "source engram"
			continue
		}
		if st, ok := endpointStatus(p.dst); !ok {
			item.Status = st
			item.Detail = "target engram"
			continue
		}
		oldWeight, err := e.store.GetAssocWeight(ctx, ws, p.src, p.dst)
		if err != nil {
			return nil, fmt.Errorf("reweight: read weight: %w", err)
		}
		info := edges[p.src][p.dst]
		item.OldWeight = oldWeight
		item.PeakWeight = info.peak
		item.CoActivationCount = info.coAct
		item.DuplicateKeys = info.dups
		if oldWeight <= 0 && info.dups == 0 {
			item.Status = "edge_not_found"
			continue
		}
		item.NewWeight = p.weight
		if dryRun {
			item.Status = "would_apply"
			continue
		}
		item.Status = "applied"
		appliedIdx = append(appliedIdx, p.idx)
		updates = append(updates, storage.AssocWeightUpdate{WS: ws, Src: p.src, Dst: p.dst, Weight: p.weight})
	}

	// Phase 5: apply in bounded batches.
	if !dryRun {
		for start := 0; start < len(updates); start += reweightApplyChunk {
			end := start + reweightApplyChunk
			if end > len(updates) {
				end = len(updates)
			}
			if err := e.store.UpdateAssocWeightBatch(ctx, updates[start:end]); err != nil {
				// Mark the unapplied tail so the caller knows exactly what landed.
				for _, idx := range appliedIdx[start:] {
					res.Items[idx].Status = "failed"
					res.Items[idx].Detail = "batch apply error"
				}
				res.Applied = start
				return res, fmt.Errorf("reweight: apply batch: %w", err)
			}
		}
		res.Applied = len(updates)
	}

	slog.Info("reweight links", "vault", vault, "requested", res.Requested, "applied", res.Applied, "dry_run", dryRun)
	return res, nil
}

// RestoreLinkWeightsDryRun scans the vault and reports how many edges a
// restore-to-peak pass at the given scale would raise, without writing.
func (e *Engine) RestoreLinkWeightsDryRun(ctx context.Context, vault string, scale float32) (*RestoreLinkWeightsResult, error) {
	if !e.beginVaultOp() {
		return nil, fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	if err := e.requireVault(vault); err != nil {
		return nil, err
	}
	ws := e.store.ResolveVaultPrefix(vault)
	counts, err := e.store.RestoreAssocWeightsToPeak(ctx, ws, scale, true, nil)
	if err != nil {
		return nil, err
	}
	return &RestoreLinkWeightsResult{
		Vault:      vault,
		Scale:      scale,
		DryRun:     true,
		Scanned:    counts.Scanned,
		WouldRaise: counts.Raised,
		Skipped:    counts.Skipped,
	}, nil
}

// StartRestoreLinkWeights raises every association's weight to peak*scale
// (where currently below that target) as a background job — the mass-recovery
// path for weights ground down to the decay floor. Peak ordering is already
// preserved by the floor; this restores amplitude. Returns a Job immediately
// (202 pattern); progress is reported via CopyCurrent (edges scanned).
func (e *Engine) StartRestoreLinkWeights(ctx context.Context, vault string, scale float32) (*vaultjob.Job, error) {
	if !e.beginVaultOp() {
		return nil, fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	mu := e.getVaultMutex(vault)
	if !mu.TryLock() {
		return nil, fmt.Errorf("vault %q: another operation is in progress", vault)
	}
	defer mu.Unlock()

	if err := e.requireVault(vault); err != nil {
		return nil, err
	}
	if scale <= 0 || scale > 1 {
		return nil, fmt.Errorf("restore scale must be in (0, 1], got %v", scale)
	}
	ws := e.store.ResolveVaultPrefix(vault)

	job, err := e.jobManager.Create("restore-link-weights", vault, vault)
	if err != nil {
		return nil, fmt.Errorf("restore-link-weights: create job: %w", err)
	}

	if !e.spawnJob(func() { e.runRestoreLinkWeights(job, ws, vault, scale) }) {
		e.jobManager.Fail(job, fmt.Errorf("engine is shutting down"))
		return job, nil
	}
	return job, nil
}

func (e *Engine) runRestoreLinkWeights(job *vaultjob.Job, ws [8]byte, vault string, scale float32) {
	defer func() {
		if r := recover(); r != nil {
			if storage.IsClosedPanic(r) {
				e.jobManager.Fail(job, fmt.Errorf("engine closed during job"))
				return
			}
			e.jobManager.Fail(job, fmt.Errorf("restore-link-weights job panicked: %v", r))
			slog.Error("restore-link-weights job panicked", "job_id", job.ID, "vault", vault, "panic", r)
		}
	}()

	counts, err := e.store.RestoreAssocWeightsToPeak(e.stopCtx, ws, scale, false, func(c storage.RestoreScanCounts) {
		job.CopyCurrent.Store(c.Scanned)
	})
	if err != nil {
		e.jobManager.Fail(job, fmt.Errorf("restore-link-weights: %w", err))
		return
	}
	job.CopyCurrent.Store(counts.Scanned)

	slog.Info("restore-link-weights complete",
		"vault", vault, "scale", scale,
		"scanned", counts.Scanned, "raised", counts.Raised, "skipped", counts.Skipped)
	e.jobManager.Complete(job)
}

// requireVault returns ErrVaultNotFound unless the vault name is registered.
func (e *Engine) requireVault(vault string) error {
	names, err := e.store.ListVaultNames()
	if err != nil {
		return fmt.Errorf("list vault names: %w", err)
	}
	for _, n := range names {
		if n == vault {
			return nil
		}
	}
	return fmt.Errorf("vault %q: %w", vault, ErrVaultNotFound)
}
