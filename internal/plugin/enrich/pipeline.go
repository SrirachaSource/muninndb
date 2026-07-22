package enrich

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/plugin/llmstats"
	"github.com/scrypster/muninndb/internal/storage"
)

// ErrNothingToEnrich is returned when all pipeline stages are skipped because
// the engram already has inline data (e.g., Summary set by caller during Write).
// This is distinct from a real failure where LLM/network errors caused stages to fail.
// Defined in the plugin package; aliased here for backwards compatibility.
var ErrNothingToEnrich = plugin.ErrNothingToEnrich

// EnrichmentPipeline orchestrates a single unified LLM call per engram. The
// call asks for entities, relationships, classification, and summary in one
// JSON response. Stages can be disabled in config (or via light mode = summary
// only); stages whose output the engram already has inline are skipped and
// carried forward. If every stage is skipped, no LLM call is made.
type EnrichmentPipeline struct {
	provider LLMProvider
	limiter  *TokenBucketLimiter
	cfg      *config.PluginConfig
	stats    llmstats.LLMCallStats
}

// NewPipeline creates a new enrichment pipeline.
// cfg may be nil, in which case all stages are enabled.
func NewPipeline(provider LLMProvider, limiter *TokenBucketLimiter) *EnrichmentPipeline {
	return &EnrichmentPipeline{
		provider: provider,
		limiter:  limiter,
	}
}

// SetConfig applies server-level enrichment configuration (per-stage flags, mode).
func (p *EnrichmentPipeline) SetConfig(cfg *config.PluginConfig) {
	p.cfg = cfg
}

// LLMStats returns a point-in-time snapshot of LLM call metrics.
func (p *EnrichmentPipeline) LLMStats() llmstats.Snapshot {
	return p.stats.Snapshot()
}

// verboseLogsFlag returns the LLMVerboseLogs config flag pointer, or nil if cfg is nil.
func (p *EnrichmentPipeline) verboseLogsFlag() *bool {
	if p.cfg == nil {
		return nil
	}
	return p.cfg.LLMVerboseLogs
}

// recordComplete updates aggregate stats and emits a verbose log entry when enabled.
func (p *EnrichmentPipeline) recordComplete(ctx context.Context, callType string, latMs int64, err error) {
	p.stats.TotalCalls.Add(1)
	p.stats.TotalLatencyMs.Add(latMs)
	if err != nil {
		p.stats.TotalErrors.Add(1)
	}
	if llmstats.VerboseEnabled(p.verboseLogsFlag()) {
		attrs := []any{
			"source", "llm",
			"subsystem", "enrich",
			"call_type", callType,
			"provider", p.provider.Name(),
			"latency_ms", latMs,
		}
		if err != nil {
			attrs = append(attrs, "error", err.Error())
		}
		slog.InfoContext(ctx, "llm.complete", attrs...)
	}
}

// stageEnabled returns whether a named stage is enabled given config and light-mode rules.
func (p *EnrichmentPipeline) stageEnabled(stage string) bool {
	if p.cfg == nil {
		return true
	}
	if p.cfg.IsLightMode() {
		return stage == "summary"
	}
	return p.cfg.EnrichStageEnabled(stage)
}

// stagesToRun returns the canonical-order list of stages that should be
// requested from the LLM for this engram: those that are config-enabled AND
// don't already have caller-provided inline data on the engram.
func (p *EnrichmentPipeline) stagesToRun(eng *storage.Engram) []string {
	out := make([]string, 0, 4)
	if p.stageEnabled("entities") && !looksFullyPreEnriched(eng) {
		out = append(out, "entities")
	}
	// Relationships depend on entities. If entities are skipped because they
	// already exist inline, we can't ask the model for relationships in a way
	// it can ground; in that case skip relationships too. If entities are
	// being asked for, ask for relationships alongside them.
	if p.stageEnabled("relationships") && p.stageEnabled("entities") && !looksFullyPreEnriched(eng) {
		out = append(out, "relationships")
	}
	if p.stageEnabled("classification") && !engramHasClassification(eng) {
		out = append(out, "classification")
	}
	if p.stageEnabled("summary") && !engramHasSummary(eng) {
		out = append(out, "summary")
	}
	return out
}

// buildUserContent assembles the per-engram USER message for one enrichment
// call. When the content looks like JSON or code, a content-type hint is
// prepended (see content_type.go) so the model treats structure AS structure.
// The hint rides the per-engram user message ONLY -- the system prompt stays
// byte-stable, so the Anthropic prompt cache (90% input discount) is preserved.
// Plain prose yields an empty hint, leaving the message byte-identical to the
// legacy format. This is the single chokepoint for both the sync Run and the
// batch EnrichBatch paths (previously duplicated inline).
func buildUserContent(eng *storage.Engram) string {
	var b strings.Builder
	if hint := contentHint(DetectContentType(eng.Content)); hint != "" {
		b.WriteString(hint)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Concept: %s\n\nContent: %s", eng.Concept, eng.Content)
	return b.String()
}

// Run executes the enrichment pipeline for one engram with a single LLM call.
//
// Stages skipped due to inline data on the engram are carried forward into the
// result so the digest is marked correctly downstream.
//
// Returns ErrNothingToEnrich (wrapped) if every stage is skipped because the
// engram already has data. Returns a real error if the LLM call fails or the
// response is unparseable.
func (p *EnrichmentPipeline) Run(ctx context.Context, eng *storage.Engram) (result *plugin.EnrichmentResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("enrich pipeline panic: %v", r)
			slog.Error("enrich: panic recovered", "panic", r)
		}
	}()

	result = &plugin.EnrichmentResult{}

	stages := p.stagesToRun(eng)
	carryForward(eng, result)

	if len(stages) == 0 {
		// Nothing for the LLM to do. Either every stage was disabled or
		// every enabled stage already has inline data. If the carry-forward
		// produced anything, return that; otherwise signal nothing-to-enrich.
		if isResultEmpty(result) {
			return nil, fmt.Errorf("engram %s: %w", eng.ID.String(), ErrNothingToEnrich)
		}
		return result, nil
	}

	if err := p.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	system := BuildUnifiedPrompt(stages)
	user := buildUserContent(eng)

	start := time.Now()
	resp, llmErr := p.provider.Complete(ctx, system, user)
	p.recordComplete(ctx, "unified", time.Since(start).Milliseconds(), llmErr)
	if llmErr != nil {
		slog.Warn("enrich: unified call failed", "id", eng.ID.String(), "err", llmErr)
		// Carry-forward may still have something useful; preserve that on
		// error rather than wiping it.
		if !isResultEmpty(result) {
			return result, nil
		}
		return nil, fmt.Errorf("enrich: unified call failed for engram %s: %w", eng.ID.String(), llmErr)
	}

	parsed, parseErr := ParseUnifiedResponse(resp, stages)
	if parseErr != nil {
		slog.Warn("enrich: unified response parse failed", "id", eng.ID.String(), "err", parseErr)
		if !isResultEmpty(result) {
			return result, nil
		}
		return nil, fmt.Errorf("enrich: unified response parse failed for engram %s: %w", eng.ID.String(), parseErr)
	}

	mergeUnified(parsed, result)
	return result, nil
}

// batchPlan is the per-engram plan carried across an EnrichBatch call: the
// stages that drove its prompt and the partially-built result (carry-forward).
type batchPlan struct {
	stages []string
	result *plugin.EnrichmentResult
}

// EnrichBatch enriches many engrams in ONE Anthropic Message Batch (50% cheaper
// on input + output). Used by the background retroactive sweep, where minutes of
// latency are fine. It is synchronous to the caller: it submits one batch and
// polls to completion before returning. Per-engram outcomes are returned in two
// maps keyed by engram-id string: successful results, and errors (parse failures,
// per-request batch failures, nothing-to-enrich). A non-nil error return means
// the WHOLE batch failed (submit/poll/fetch) and the caller should retry the set.
//
// Returns ErrBatchUnsupported if the provider is not batch-capable — the caller
// must fall back to the synchronous Enrich() path.
func (p *EnrichmentPipeline) EnrichBatch(
	ctx context.Context, engs []*storage.Engram,
) (results map[string]*plugin.EnrichmentResult, errs map[string]error, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("enrich batch panic: %v", r)
			slog.Error("enrich: batch panic recovered", "panic", r)
		}
	}()

	bp, ok := p.provider.(BatchLLMProvider)
	if !ok {
		return nil, nil, ErrBatchUnsupported
	}

	results = make(map[string]*plugin.EnrichmentResult, len(engs))
	errs = make(map[string]error, len(engs))

	// Plan each engram exactly like Run: stage selection + carry-forward. Only
	// engrams with stages to run become batch items; the rest resolve inline.
	plans := make(map[string]*batchPlan, len(engs))
	items := make([]BatchItem, 0, len(engs))
	for _, eng := range engs {
		id := eng.ID.String()
		res := &plugin.EnrichmentResult{}
		stages := p.stagesToRun(eng)
		carryForward(eng, res)
		if len(stages) == 0 {
			if isResultEmpty(res) {
				errs[id] = fmt.Errorf("engram %s: %w", id, ErrNothingToEnrich)
			} else {
				results[id] = res
			}
			continue
		}
		plans[id] = &batchPlan{stages: stages, result: res}
		items = append(items, BatchItem{
			CustomID: id,
			System:   BuildUnifiedPrompt(stages),
			User:     buildUserContent(eng),
		})
	}

	if len(items) == 0 {
		return results, errs, nil
	}

	// One HTTP submit for the whole batch — gate it through the limiter once.
	if werr := p.limiter.Wait(ctx); werr != nil {
		return results, errs, werr
	}

	start := time.Now()
	batchID, serr := bp.SubmitBatch(ctx, items)
	if serr != nil {
		return results, errs, fmt.Errorf("submit batch: %w", serr)
	}
	slog.Info("enrich: batch submitted", "batch_id", batchID, "requests", len(items))

	resultsURL, perr := pollBatchToCompletion(ctx, bp, batchID)
	if perr != nil {
		return results, errs, perr
	}

	fetched, ferr := bp.FetchBatchResults(ctx, resultsURL)
	if ferr != nil {
		return results, errs, fmt.Errorf("fetch batch results: %w", ferr)
	}
	slog.Info("enrich: batch complete",
		"batch_id", batchID, "elapsed_ms", time.Since(start).Milliseconds(),
		"requests", len(items), "fetched", len(fetched))

	// Map each result back and finish it the same way Run does (parse + merge).
	for id, plan := range plans {
		br, present := fetched[id]
		if !present {
			errs[id] = fmt.Errorf("engram %s: missing from batch results", id)
			continue
		}
		if br.Err != "" {
			if !isResultEmpty(plan.result) {
				results[id] = plan.result // carry-forward salvage
			} else {
				errs[id] = fmt.Errorf("engram %s: batch request %s", id, br.Err)
			}
			continue
		}
		parsed, parseErr := ParseUnifiedResponse(br.Text, plan.stages)
		if parseErr != nil {
			if !isResultEmpty(plan.result) {
				results[id] = plan.result
			} else {
				errs[id] = fmt.Errorf("enrich: batch parse failed for engram %s: %w", id, parseErr)
			}
			continue
		}
		mergeUnified(parsed, plan.result)
		results[id] = plan.result
	}

	return results, errs, nil
}

// pollBatchToCompletion polls a submitted batch until it ends, returning the
// results URL. Caps total wait so a stuck batch cannot block a sweep forever;
// on timeout the caller leaves the engrams un-flagged and they retry next sweep.
func pollBatchToCompletion(ctx context.Context, bp BatchLLMProvider, batchID string) (string, error) {
	const (
		pollInterval = 10 * time.Second
		maxWait      = 30 * time.Minute
	)
	deadline := time.Now().Add(maxWait)
	for {
		st, err := bp.PollBatch(ctx, batchID)
		if err != nil {
			return "", fmt.Errorf("poll batch %s: %w", batchID, err)
		}
		if st.Ended {
			if st.ResultsURL == "" {
				return "", fmt.Errorf("batch %s ended without results_url", batchID)
			}
			return st.ResultsURL, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("batch %s did not complete within %s", batchID, maxWait)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// carryForward populates `result` with stage outputs that already live on the
// engram (caller-provided inline data). PersistEnrichmentResult -> UpdateDigest
// uses these to set the corresponding Digest* flags.
func carryForward(eng *storage.Engram, result *plugin.EnrichmentResult) {
	if engramHasClassification(eng) {
		result.MemoryType = eng.MemoryType.String()
		result.TypeLabel = eng.TypeLabel
	}
	if engramHasSummary(eng) {
		result.Summary = eng.Summary
		result.KeyPoints = eng.KeyPoints
	}
}

// mergeUnified copies parsed LLM output into result for the fields the model
// was asked about, mapping classification strings into the canonical storage
// types. Carry-forward fields already on result are preserved when the model
// returned nothing for that stage.
func mergeUnified(parsed *UnifiedEnrichment, result *plugin.EnrichmentResult) {
	if len(parsed.Entities) > 0 {
		result.Entities = parsed.Entities
	}
	if len(parsed.Relationships) > 0 {
		result.Relationships = parsed.Relationships
	}
	if parsed.MemoryType != "" || parsed.TypeLabel != "" || parsed.Category != "" || parsed.Subcategory != "" {
		mt, _ := resolveClassification(parsed.MemoryType, parsed.TypeLabel)
		result.MemoryType = mt.String()
		result.TypeLabel = parsed.TypeLabel
		if parsed.Category != "" && parsed.Subcategory != "" {
			result.Classification = parsed.Category + "/" + parsed.Subcategory
		}
	}
	if parsed.Summary != "" {
		result.Summary = parsed.Summary
	}
	if len(parsed.KeyPoints) > 0 {
		result.KeyPoints = parsed.KeyPoints
	}
}

// isResultEmpty reports whether an EnrichmentResult has no usable output at all.
func isResultEmpty(r *plugin.EnrichmentResult) bool {
	return r.Summary == "" &&
		len(r.KeyPoints) == 0 &&
		len(r.Entities) == 0 &&
		len(r.Relationships) == 0 &&
		r.MemoryType == "" &&
		r.TypeLabel == "" &&
		r.Classification == ""
}

// looksFullyPreEnriched is a PROXY heuristic for "the caller provided a fully
// pre-enriched engram". It checks KeyPoints+Summary because the Engram struct
// carries neither entity links nor digest flags, so the pipeline cannot ask
// the authoritative question ("did the caller declare entities?") from here —
// only the DigestEntities flag knows, and only the store has it.
//
// CAVEAT: this is WRONG for the common caller who declares entities + summary
// but no key points — for those the entities stage still runs and the LLM's
// guesses are stopped only at persist time by the DigestEntities flag guards
// (retroactive.go processEnrichEngram, engine_replay.go). The waste (an LLM
// asked for entities that will be discarded) is real; the damage is not,
// provided the flag guards hold.
//
// Renamed from engramHasEntities, which claimed to check entities and did not
// (issue #80, enrich half).
func looksFullyPreEnriched(eng *storage.Engram) bool {
	return len(eng.KeyPoints) > 0 && eng.Summary != ""
}

// engramHasSummary returns true if the engram already has a caller-provided summary.
func engramHasSummary(eng *storage.Engram) bool {
	return eng.Summary != ""
}

// engramHasClassification returns true if the engram already has a non-default MemoryType
// set by the caller (anything beyond the zero-value TypeFact with a TypeLabel).
func engramHasClassification(eng *storage.Engram) bool {
	return eng.MemoryType != storage.TypeFact || eng.TypeLabel != ""
}

// memoryTypeNames maps LLM classification output strings to storage.MemoryType values.
var memoryTypeNames = map[string]storage.MemoryType{
	"fact":        storage.TypeFact,
	"decision":    storage.TypeDecision,
	"observation": storage.TypeObservation,
	"preference":  storage.TypePreference,
	"issue":       storage.TypeIssue,
	"bugfix":      storage.TypeIssue,
	"bug_report":  storage.TypeIssue,
	"task":        storage.TypeTask,
	"procedure":   storage.TypeProcedure,
	"event":       storage.TypeEvent,
	"experience":  storage.TypeEvent,
	"goal":        storage.TypeGoal,
	"constraint":  storage.TypeConstraint,
	"identity":    storage.TypeIdentity,
	"reference":   storage.TypeReference,
}

// resolveClassification maps the LLM's memory_type and type_label strings to
// the storage.MemoryType enum and a display string. The display string prefers
// type_label if present, otherwise falls back to the canonical enum name.
func resolveClassification(memType, typeLabel string) (storage.MemoryType, string) {
	mt, ok := memoryTypeNames[memType]
	if !ok {
		mt = storage.TypeFact
	}
	if typeLabel != "" {
		return mt, typeLabel
	}
	return mt, memType
}

