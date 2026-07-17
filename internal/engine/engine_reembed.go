package engine

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/storage"
)

// StartReembedVault clears stale embeddings and digest flags for the named vault,
// allowing the RetroactiveProcessor to re-embed every engram with the current model.
//
// The pipeline:
//  1. Reset in-memory HNSW index for vault
//  2. Clear all 0x07 HNSW keys (range tombstone)
//  3. Clear all 0x18 embedding keys + reset DigestEmbed flag on every engram
//  4. Clear embed model marker
//  5. Notify RetroactiveProcessor via onWrite callback
//
// Returns a Job immediately (202 pattern). Flag clearing completes in the background
// goroutine (typically seconds). Actual re-embedding is handled by the existing
// RetroactiveProcessor micro-batch pipeline.
func (e *Engine) StartReembedVault(ctx context.Context, vaultName, modelName string) (*vaultjob.Job, error) {
	if !e.beginVaultOp() {
		return nil, fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	mu := e.getVaultMutex(vaultName)
	if !mu.TryLock() {
		return nil, fmt.Errorf("vault %q: another operation is in progress", vaultName)
	}
	defer mu.Unlock()

	// Verify vault exists.
	names, err := e.store.ListVaultNames()
	if err != nil {
		return nil, fmt.Errorf("reembed: list vault names: %w", err)
	}
	found := false
	for _, n := range names {
		if n == vaultName {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("vault %q: %w", vaultName, ErrVaultNotFound)
	}

	ws := e.store.VaultPrefix(vaultName)

	// Count engrams to set progress totals.
	engramCount := e.store.GetVaultCount(ctx, ws)

	job, err := e.jobManager.Create("reembed", vaultName, vaultName)
	if err != nil {
		return nil, fmt.Errorf("reembed: create job: %w", err)
	}
	job.CopyTotal = engramCount // "copy" phase = flag clearing
	job.IndexTotal = 0          // no separate index phase; RetroactiveProcessor handles it

	if !e.spawnJob(func() { e.runReembed(job, ws, vaultName) }) {
		e.jobManager.Fail(job, fmt.Errorf("engine is shutting down"))
		return job, nil // job is already failed; return it so the caller can report the job_id
	}
	return job, nil
}

func (e *Engine) runReembed(job *vaultjob.Job, ws [8]byte, vaultName string) {
	ctx := e.stopCtx

	defer func() {
		if r := recover(); r != nil {
			// Swallow closed-DB panics — can occur if the 30s Stop() timeout
			// expires and Pebble is closed before this goroutine exits.
			if storage.IsClosedPanic(r) {
				e.jobManager.Fail(job, fmt.Errorf("engine closed during job"))
				return
			}
			e.jobManager.Fail(job, fmt.Errorf("reembed job panicked: %v", r))
			slog.Error("reembed job panicked", "job_id", job.ID, "vault", vaultName, "panic", r)
		}
	}()

	// Step 1: Reset in-memory HNSW index for this vault.
	if e.hnswRegistry != nil {
		e.hnswRegistry.ResetVault(ws)
	}

	// Step 2: Clear all 0x07 HNSW keys.
	if err := e.store.ClearHNSWForVault(ws); err != nil {
		e.jobManager.Fail(job, fmt.Errorf("clear HNSW: %w", err))
		return
	}

	// Step 3: Clear all 0x18 embedding keys + reset DigestEmbed flags.
	cleared, err := e.store.ClearEmbedFlagsForVault(ctx, ws)
	if err != nil {
		e.jobManager.Fail(job, fmt.Errorf("clear embed flags: %w", err))
		return
	}
	job.CopyCurrent.Store(cleared)

	// Step 4: Clear embed model marker so it gets re-set on next embed cycle.
	if err := e.store.SetEmbedModel(ws, ""); err != nil {
		slog.Warn("reembed: failed to clear embed model marker", "vault", vaultName, "err", err)
		// Non-fatal — the processor will still re-embed everything.
	}

	// Step 5: Notify RetroactiveProcessor to wake up and start re-embedding.
	if fn, ok := e.onWrite.Load().(func()); ok && fn != nil {
		fn()
	}

	slog.Info("reembed: flags cleared, RetroactiveProcessor will re-embed in background",
		"vault", vaultName, "flags_cleared", cleared)

	e.jobManager.Complete(job)
}

// StartReembedMissing clears embed digest flags for ONLY the engrams in the
// vault that have no embedding (EmbedDim == 0) and wakes the
// RetroactiveProcessor to backfill them. Existing vectors, the HNSW index, and
// the embed model marker are untouched — recall stays fully semantic on the
// covered engrams while the gap re-embeds. Use this for partial-coverage
// remediation; StartReembedVault remains the model-migration full rebuild.
func (e *Engine) StartReembedMissing(ctx context.Context, vaultName string) (*vaultjob.Job, error) {
	if !e.beginVaultOp() {
		return nil, fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	mu := e.getVaultMutex(vaultName)
	if !mu.TryLock() {
		return nil, fmt.Errorf("vault %q: another operation is in progress", vaultName)
	}
	defer mu.Unlock()

	names, err := e.store.ListVaultNames()
	if err != nil {
		return nil, fmt.Errorf("reembed-missing: list vault names: %w", err)
	}
	found := false
	for _, n := range names {
		if n == vaultName {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("vault %q: %w", vaultName, ErrVaultNotFound)
	}

	ws := e.store.VaultPrefix(vaultName)

	job, err := e.jobManager.Create("reembed-missing", vaultName, vaultName)
	if err != nil {
		return nil, fmt.Errorf("reembed-missing: create job: %w", err)
	}
	job.CopyTotal = e.store.GetVaultCount(ctx, ws) // scan upper bound
	job.IndexTotal = 0

	if !e.spawnJob(func() { e.runReembedMissing(job, ws, vaultName) }) {
		e.jobManager.Fail(job, fmt.Errorf("engine is shutting down"))
		return job, nil
	}
	return job, nil
}

func (e *Engine) runReembedMissing(job *vaultjob.Job, ws [8]byte, vaultName string) {
	ctx := e.stopCtx

	defer func() {
		if r := recover(); r != nil {
			if storage.IsClosedPanic(r) {
				e.jobManager.Fail(job, fmt.Errorf("engine closed during job"))
				return
			}
			e.jobManager.Fail(job, fmt.Errorf("reembed-missing job panicked: %v", r))
			slog.Error("reembed-missing job panicked", "job_id", job.ID, "vault", vaultName, "panic", r)
		}
	}()

	cleared, err := e.store.ClearEmbedFlagsForMissing(ctx, ws)
	if err != nil {
		e.jobManager.Fail(job, fmt.Errorf("clear embed flags (missing): %w", err))
		return
	}
	job.CopyCurrent.Store(cleared)

	// Wake the RetroactiveProcessor to backfill the cleared engrams.
	if fn, ok := e.onWrite.Load().(func()); ok && fn != nil {
		fn()
	}

	slog.Info("reembed-missing: flags cleared for unembedded engrams; RetroactiveProcessor will backfill",
		"vault", vaultName, "flags_cleared", cleared)
	e.jobManager.Complete(job)
}
