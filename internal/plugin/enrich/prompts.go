package enrich

import (
	"sort"
	"strings"
)

// unifiedPromptHeader is the static intro for the unified enrichment prompt.
// Kept identical across all engrams so it can be cached by Anthropic's prompt
// cache (cache_control: ephemeral). Caches at 5-min or 1h TTL when the
// extended-cache-ttl-2025-04-11 beta header is set on the request.
const unifiedPromptHeader = `You are a memory enrichment system. Given a memory's concept and content, produce a SINGLE JSON object containing all of the requested fields below.

Return ONLY valid JSON. No explanation text. No markdown fences.

The shape of the JSON object is built dynamically from the enabled stages listed below. Include ONLY the fields for enabled stages; omit fields for any stage not listed.

`

// unifiedStageBlocks holds the per-stage instructions for the unified prompt.
// Each block is identical bytes across every engram so the whole assembled
// system prompt is cache-friendly.
var unifiedStageBlocks = map[string]string{
	"entities": `## entities
Field: "entities": [{"name": "entity name", "type": "entity_type", "confidence": 0.95}]

Extract ALL named entities from the content, even briefly mentioned.
Entity types (use exactly these strings): person, organization, project, tool, framework, language, database, service, technology, location, concept, product, event, other.

Use the most specific entity name (e.g., "PostgreSQL" not "database").
Confidence: 1.0 = explicitly named, 0.7 = strongly implied, 0.4 = loosely mentioned.
Return an empty array if none.

`,
	"relationships": `## relationships
Field: "relationships": [{"from": "entity_a", "to": "entity_b", "type": "relationship_type", "weight": 0.8}]

Only create relationships between entities that appear in the "entities" field you produced above. Skip otherwise.
Relationship types: manages, uses, depends_on, implements, created_by, belongs_to, part_of, integrates_with, deployed_on, alternative_to.
Weight: 1.0 = explicitly stated, 0.6 = strongly implied, 0.3 = loosely inferred.
Return an empty array if none.

`,
	"classification": `## classification
Fields:
  "memory_type":  one of: fact, decision, observation, preference, issue, task, procedure, event, goal, constraint, identity, reference
  "type_label":   more specific snake_case label (e.g., architectural_decision, coding_pattern, meeting_notes, bug_report, api_design)
  "category":     broad topic (e.g., infrastructure, authentication, team)
  "subcategory":  specific (e.g., databases, JWT, hiring)
  "tags":         2-5 lowercase keyword strings for search

`,
	"summary": `## summary
Fields:
  "summary":     1-3 sentence abstractive summary capturing essential meaning. Do not start with "This memory..." or "The text discusses...". Present tense for facts, past tense for events.
  "key_points":  array of 3-7 self-contained statement strings of fact or decision.

`,
}

// BuildUnifiedPrompt returns the system prompt for a single LLM call covering
// only the enabled stages. The output is byte-stable for a given set of stages
// so repeated calls hit the prompt cache.
//
// `stages` may be in any order and may contain duplicates; the result is
// canonicalised. An empty input yields an empty string — the caller should
// skip the LLM call entirely in that case.
func BuildUnifiedPrompt(stages []string) string {
	canonical := canonicalStageOrder(stages)
	if len(canonical) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(unifiedPromptHeader)
	b.WriteString("Enabled stages: ")
	b.WriteString(strings.Join(canonical, ", "))
	b.WriteString("\n\n")
	for _, stage := range canonical {
		if block, ok := unifiedStageBlocks[stage]; ok {
			b.WriteString(block)
		}
	}
	b.WriteString(`Output: a single JSON object with exactly the fields listed above for the enabled stages, and no others.`)
	return b.String()
}

// canonicalStageOrder returns the input stages deduped and ordered by the
// canonical pipeline order: entities, relationships, classification, summary.
// Unknown stage names are dropped.
func canonicalStageOrder(stages []string) []string {
	order := map[string]int{
		"entities":       0,
		"relationships":  1,
		"classification": 2,
		"summary":        3,
	}
	seen := make(map[string]bool, len(stages))
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		if _, ok := order[s]; !ok {
			continue
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}
