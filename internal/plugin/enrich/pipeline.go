package enrich

import (
	"context"
	"fmt"
	"log/slog"
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
	if p.stageEnabled("entities") && !engramHasEntities(eng) {
		out = append(out, "entities")
	}
	// Relationships depend on entities. If entities are skipped because they
	// already exist inline, we can't ask the model for relationships in a way
	// it can ground; in that case skip relationships too. If entities are
	// being asked for, ask for relationships alongside them.
	if p.stageEnabled("relationships") && p.stageEnabled("entities") && !engramHasEntities(eng) {
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
	user := fmt.Sprintf("Concept: %s\n\nContent: %s", eng.Concept, eng.Content)

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

// engramHasEntities returns true if the engram already has caller-provided entities,
// used as a skip-if-present guard in pipeline.Run for inline enrichment only.
// The retroactive processor uses GetDigestFlags (DigestEntities flag) instead of this check.
// This heuristic: only skip if both KeyPoints AND Summary are present, indicating the
// caller provided a fully pre-enriched engram.
func engramHasEntities(eng *storage.Engram) bool {
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

