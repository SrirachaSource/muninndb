package storage

import (
	"context"
	"fmt"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// RestoreScanCounts summarizes a RestoreAssocWeightsToPeak pass.
type RestoreScanCounts struct {
	Scanned int64 // forward keys visited
	Raised  int64 // edges whose weight was (or would be) raised to peak*scale
	Skipped int64 // edges already at/above peak*scale, or with no recorded peak
}

// restoreFlushSize bounds how many weight updates are applied per Pebble batch.
const restoreFlushSize = 1000

// restoreProgressEvery controls how often onProgress fires during the walk.
const restoreProgressEvery = 5000

// RestoreAssocWeightsToPeak walks every forward association in the vault and
// raises Weight to PeakWeight*scale wherever the current weight sits below that
// target (clamped to 1.0). It never lowers a weight: edges already at or above
// the target — e.g. recently co-activated or LTP-floored pairs — are skipped,
// as are edges with no recorded peak. Because PeakWeight is monotone, this
// recovers amplitude lost to weight decay while preserving the peak ordering.
//
// Updates route through UpdateAssocWeightBatch, so old-weight keys are deleted
// (no ghost accumulation from this path), metadata is preserved, and the assoc
// cache is invalidated. Each raised edge is stamped restoredAt=now (unless it
// already carries a stamp), so the engine's re-establishment rules govern the
// restored weight — it is a restore, not an earned strength. dryRun performs
// the identical walk and returns the counts without writing. onProgress
// (optional) is invoked periodically with the running counts.
func (ps *PebbleStore) RestoreAssocWeightsToPeak(ctx context.Context, wsPrefix [8]byte, scale float32, dryRun bool, onProgress func(c RestoreScanCounts)) (RestoreScanCounts, error) {
	var c RestoreScanCounts
	if scale <= 0 || scale > 1 {
		return c, fmt.Errorf("restore scale must be in (0, 1], got %v", scale)
	}

	lower := keys.AssocFwdRangeStart(wsPrefix)
	upper := keys.AssocFwdRangeEnd(wsPrefix)
	iter, err := ps.pebbleReader(ctx).NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		return c, fmt.Errorf("restore: assoc iterator: %w", err)
	}
	defer iter.Close()

	var pending []AssocWeightUpdate
	flush := func() error {
		if dryRun || len(pending) == 0 {
			pending = pending[:0]
			return nil
		}
		if err := ps.UpdateAssocWeightBatch(ctx, pending); err != nil {
			return fmt.Errorf("restore: apply batch: %w", err)
		}
		pending = pending[:0]
		return nil
	}

	for iter.First(); iter.Valid(); iter.Next() {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		k := iter.Key()
		// Key layout: 0x03 | ws(8) | srcID(16) | weightComplement(4) | dstID(16) = 45 bytes.
		if len(k) < 45 {
			continue
		}
		c.Scanned++

		var wc [4]byte
		copy(wc[:], k[25:29])
		weight := keys.WeightFromComplement(wc)
		_, _, _, _, peak, _, _ := decodeAssocValue(iter.Value())

		target := peak * scale
		if target > 1.0 {
			target = 1.0
		}
		if peak <= 0 || weight >= target {
			c.Skipped++
		} else {
			c.Raised++
			if !dryRun {
				var src, dst ULID
				copy(src[:], k[9:25])
				copy(dst[:], k[29:45])
				pending = append(pending, AssocWeightUpdate{
					WS:            wsPrefix,
					Src:           src,
					Dst:           dst,
					Weight:        target,
					SetRestoredAt: true,
				})
				if len(pending) >= restoreFlushSize {
					if err := flush(); err != nil {
						return c, err
					}
				}
			}
		}

		if onProgress != nil && c.Scanned%restoreProgressEvery == 0 {
			onProgress(c)
		}
	}

	if err := flush(); err != nil {
		return c, err
	}
	if onProgress != nil {
		onProgress(c)
	}
	return c, nil
}
