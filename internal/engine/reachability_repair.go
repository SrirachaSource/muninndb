package engine

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/scrypster/muninndb/internal/storage"
)

// ReachabilityRepairData is one page of a reachability sweep: every graph
// node in the window self-probed, the unreachable ones repaired (re-inserted
// through the in-link-guaranteed insert path) unless dry-run, and a cursor
// for the next page. The sweep exists because a node CAN sit in the graph
// with out-edges only — born in-link-starved before the insert-time
// guarantee, and therefore invisible to Search — and no rebuild-free surface
// could heal that on a live server.
type ReachabilityRepairData struct {
	Vault       string
	ProbeK      int
	Limit       int
	Scanned     int
	Unreachable []string // ids that failed the self-probe this page
	Repaired    int      // re-inserts performed (0 on dry-run)
	Verified    int      // repaired ids whose post-repair self-probe found them
	Failed      []string // repaired ids still unreachable after re-insert
	Skipped     []string // unreachable ids the repair refused (e.g. entry point)
	NextAfter   string   // cursor: pass as `after` for the next page ("" when done)
	Done        bool
	DryRun      bool
}

// ReachabilityRepair self-probes up to limit graph nodes after the `after`
// cursor and, unless dryRun, re-inserts each unreachable node so the fixed
// insert path wires it a guaranteed layer-0 in-link. Paged: one Search per
// node is the dominant cost, so the caller loops on NextAfter until Done.
// Read-only when dryRun — safe to census a live vault.
func (e *Engine) ReachabilityRepair(ctx context.Context, vault string, probeK, limit int, after string, dryRun bool) (*ReachabilityRepairData, error) {
	if e.hnswRegistry == nil {
		return nil, fmt.Errorf("reachability-repair: no hnsw registry")
	}
	if probeK <= 0 {
		probeK = 20
	}
	if limit <= 0 {
		limit = 500
	}
	wsPrefix := e.store.ResolveVaultPrefix(vault)

	var afterID storage.ULID
	haveAfter := false
	if after != "" {
		id, err := storage.ParseULID(after)
		if err != nil {
			return nil, fmt.Errorf("reachability-repair: parse after: %w", err)
		}
		afterID = id
		haveAfter = true
	}

	all := e.hnswRegistry.NodeIDs(wsPrefix)
	data := &ReachabilityRepairData{Vault: vault, ProbeK: probeK, Limit: limit, DryRun: dryRun}

	start := 0
	if haveAfter {
		start = sort.Search(len(all), func(i int) bool {
			return bytes.Compare(all[i][:], afterID[:]) > 0
		})
	}

	i := start
	for ; i < len(all) && data.Scanned < limit; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := all[i]
		data.Scanned++

		found, _, err := e.hnswRegistry.SelfProbe(ctx, wsPrefix, id, probeK)
		if err != nil {
			return nil, fmt.Errorf("reachability-repair: self-probe %s: %w", storage.ULID(id).String(), err)
		}
		if found {
			continue
		}
		ulidStr := storage.ULID(id).String()
		data.Unreachable = append(data.Unreachable, ulidStr)
		if dryRun {
			continue
		}

		ok, rerr := e.hnswRegistry.Reinsert(wsPrefix, id)
		if rerr != nil || !ok {
			// The entry point (reachable by construction — a probe can't miss
			// it) or a node that vanished mid-sweep: record, don't abort the
			// page over one specimen.
			data.Skipped = append(data.Skipped, ulidStr)
			continue
		}
		data.Repaired++

		refound, _, perr := e.hnswRegistry.SelfProbe(ctx, wsPrefix, id, probeK)
		if perr != nil {
			return nil, fmt.Errorf("reachability-repair: verify probe %s: %w", ulidStr, perr)
		}
		if refound {
			data.Verified++
		} else {
			data.Failed = append(data.Failed, ulidStr)
		}
	}

	if i >= len(all) {
		data.Done = true
	} else {
		data.NextAfter = storage.ULID(all[i-1]).String()
	}
	return data, nil
}
