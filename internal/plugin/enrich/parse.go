package enrich

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/scrypster/muninndb/internal/plugin"
)

// knownEntityTypes mirrors the entity types recognised by the UI colour map
// in web/static/js/app.js:getEntityTypeColor. Extend both together.
var knownEntityTypes = map[string]bool{
	"person":       true,
	"organization": true,
	"project":      true,
	"tool":         true,
	"framework":    true,
	"language":     true,
	"database":     true,
	"service":      true,
	"technology":   true,
	"location":     true,
	"concept":      true,
	"product":      true,
	"event":        true,
	"other":        true,
}

// extractJSON finds and returns the first valid JSON structure in a string.
// Handles markdown code fences and trailing text.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)

	// Remove markdown code fences if present
	if strings.Contains(s, "```json") {
		start := strings.Index(s, "```json")
		end := strings.Index(s[start+7:], "```")
		if end != -1 {
			s = s[start+7 : start+7+end]
			s = strings.TrimSpace(s)
		}
	} else if strings.Contains(s, "```") {
		start := strings.Index(s, "```")
		end := strings.Index(s[start+3:], "```")
		if end != -1 {
			s = s[start+3 : start+3+end]
			s = strings.TrimSpace(s)
		}
	}

	// Find first [ or {
	start := strings.IndexAny(s, "[{")
	if start < 0 {
		return s
	}

	// Walk forward with a bracket-depth counter to find the end of the first
	// complete JSON object or array. A backwards scan would incorrectly grab
	// both objects when a model (e.g. llama3.2) repeats its output. The depth
	// walk also correctly skips brackets inside quoted strings.
	open := s[start]
	var close byte
	if open == '{' {
		close = '}'
	} else {
		close = ']'
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if c == open {
			depth++
		} else if c == close {
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[start : i+1])
			}
		}
	}

	return s[start:]
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// UnifiedEnrichment holds the parsed output of a single unified enrichment
// call. Field strings (MemoryType, TypeLabel, Category, Subcategory) carry the
// raw LLM output; callers are responsible for mapping them to canonical
// storage types via resolveClassification in pipeline.go.
type UnifiedEnrichment struct {
	Entities      []plugin.ExtractedEntity
	Relationships []plugin.ExtractedRelation
	MemoryType    string
	TypeLabel     string
	Category      string
	Subcategory   string
	Tags          []string
	Summary       string
	KeyPoints     []string
}

// ParseUnifiedResponse parses the single-call enrichment response into a
// UnifiedEnrichment value, populating only the fields for the enabled stages.
//
// Fields not listed in `enabled` are left zero-valued even if the LLM
// returned them. Relationships whose endpoints are not present in the parsed
// entity set are dropped (defense against the model hallucinating links to
// entities it failed to extract).
//
// Returns an error only if the response is not valid JSON or every enabled
// stage produced an empty payload. Partial output is preserved: e.g. if
// entities parses cleanly but classification is missing, the returned
// UnifiedEnrichment has entities populated and classification fields blank.
func ParseUnifiedResponse(raw string, enabled []string) (*UnifiedEnrichment, error) {
	raw = strings.TrimSpace(raw)
	jsonStr := extractJSON(raw)

	var wrapper struct {
		Entities      json.RawMessage `json:"entities"`
		Relationships json.RawMessage `json:"relationships"`
		MemoryType    string          `json:"memory_type"`
		TypeLabel     string          `json:"type_label"`
		Category      string          `json:"category"`
		Subcategory   string          `json:"subcategory"`
		Tags          []string        `json:"tags"`
		Summary       string          `json:"summary"`
		KeyPoints     []string        `json:"key_points"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
		return nil, fmt.Errorf("invalid unified enrichment JSON: %s", truncateForError(jsonStr))
	}

	result := &UnifiedEnrichment{}
	want := func(stage string) bool {
		for _, s := range enabled {
			if s == stage {
				return true
			}
		}
		return false
	}

	if want("entities") && len(wrapper.Entities) > 0 && !isJSONNull(wrapper.Entities) {
		var ents []plugin.ExtractedEntity
		if err := json.Unmarshal(wrapper.Entities, &ents); err == nil {
			result.Entities = validateAndDedupeEntities(ents)
		}
	}

	if want("relationships") && len(wrapper.Relationships) > 0 && !isJSONNull(wrapper.Relationships) {
		var rels []struct {
			From   string  `json:"from"`
			To     string  `json:"to"`
			Type   string  `json:"type"`
			Weight float32 `json:"weight"`
		}
		if err := json.Unmarshal(wrapper.Relationships, &rels); err == nil {
			parsed := make([]plugin.ExtractedRelation, 0, len(rels))
			for _, r := range rels {
				parsed = append(parsed, plugin.ExtractedRelation{
					FromEntity: r.From,
					ToEntity:   r.To,
					RelType:    r.Type,
					Weight:     r.Weight,
				})
			}
			parsed = validateRelationships(parsed)
			// Drop hallucinated relationships that don't anchor to a parsed entity.
			if len(result.Entities) > 0 {
				known := make(map[string]bool, len(result.Entities))
				for _, e := range result.Entities {
					known[e.Name] = true
				}
				filtered := parsed[:0]
				for _, r := range parsed {
					if known[r.FromEntity] && known[r.ToEntity] {
						filtered = append(filtered, r)
					}
				}
				parsed = filtered
			}
			result.Relationships = parsed
		}
	}

	if want("classification") {
		result.MemoryType = wrapper.MemoryType
		result.TypeLabel = wrapper.TypeLabel
		result.Category = wrapper.Category
		result.Subcategory = wrapper.Subcategory
		result.Tags = wrapper.Tags
	}

	if want("summary") {
		result.Summary = wrapper.Summary
		result.KeyPoints = wrapper.KeyPoints
	}

	if isUnifiedResultEmpty(result) {
		return nil, fmt.Errorf("unified enrichment response had no usable fields: %s", truncateForError(jsonStr))
	}
	return result, nil
}

// isUnifiedResultEmpty returns true when none of the requested fields have any
// content, so callers can distinguish "model returned nothing" from "model
// returned partial data".
func isUnifiedResultEmpty(r *UnifiedEnrichment) bool {
	return len(r.Entities) == 0 &&
		len(r.Relationships) == 0 &&
		r.MemoryType == "" &&
		r.TypeLabel == "" &&
		r.Category == "" &&
		r.Subcategory == "" &&
		len(r.Tags) == 0 &&
		r.Summary == "" &&
		len(r.KeyPoints) == 0
}

func truncateForError(s string) string {
	const maxLen = 160
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// validateAndDedupeEntities validates entity fields and removes duplicates (keeping highest confidence).
func validateAndDedupeEntities(entities []plugin.ExtractedEntity) []plugin.ExtractedEntity {
	seen := make(map[string]plugin.ExtractedEntity)

	for _, e := range entities {
		e.Name = strings.TrimSpace(e.Name)
		e.Type = strings.TrimSpace(e.Type)

		// Skip empty names
		if e.Name == "" {
			continue
		}

		// Validate and normalize type
		e.Type = normalizeEntityType(e.Type)

		// Clamp confidence to [0.0, 1.0]
		if e.Confidence < 0.0 {
			e.Confidence = 0.0
		} else if e.Confidence > 1.0 {
			e.Confidence = 1.0
		}

		// Keep highest confidence for duplicates
		if existing, ok := seen[e.Name]; ok {
			if e.Confidence > existing.Confidence {
				seen[e.Name] = e
			}
		} else {
			seen[e.Name] = e
		}
	}

	result := make([]plugin.ExtractedEntity, 0, len(seen))
	for _, e := range seen {
		result = append(result, e)
	}

	return result
}

// normalizeEntityType normalizes entity type strings to lowercase and
// validates against the known types recognised by the UI colour map.
// Known types are returned as-is after normalisation. Unknown types are
// returned as their normalised string rather than being silently coerced
// to "service", which would corrupt semantic information and cause the
// graph UI to display incorrect colours for any type not in the original
// eight-item allowlist (e.g. "technology", "location", "concept", "event").
func normalizeEntityType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))

	if knownEntityTypes[t] || t == "" {
		return t
	}

	// Pass through unrecognised types rather than coercing to "service".
	// This preserves the LLM's semantic intent and avoids silent data
	// corruption when new types are added to the UI before the allowlist
	// is updated.
	return t
}

// validateRelationships validates relationship fields.
func validateRelationships(rels []plugin.ExtractedRelation) []plugin.ExtractedRelation {
	result := make([]plugin.ExtractedRelation, 0, len(rels))

	for _, r := range rels {
		r.FromEntity = strings.TrimSpace(r.FromEntity)
		r.ToEntity = strings.TrimSpace(r.ToEntity)
		r.RelType = strings.TrimSpace(r.RelType)

		// Skip if from or to is empty
		if r.FromEntity == "" || r.ToEntity == "" {
			continue
		}

		// Clamp weight to [0.0, 1.0]
		if r.Weight < 0.0 {
			r.Weight = 0.0
		} else if r.Weight > 1.0 {
			r.Weight = 1.0
		}

		result = append(result, r)
	}

	return result
}
