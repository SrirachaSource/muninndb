package auth

import "fmt"

// RecallModePreset is a bundle of recall parameters for common retrieval patterns.
// Zero-value fields mean "do not override caller defaults".
type RecallModePreset struct {
	MaxHops            int
	Threshold          float32
	SemanticSimilarity float32
	FullTextRelevance  float32
	Recency            float32
	DisableACTR        bool
}

// recallModePresets are the canonical recall mode definitions, shared by MCP and REST handlers.
//
// NOTE on Threshold: it is the candidate-retrieval SIMILARITY floor (0-1 cosine
// scale), applied BEFORE ACT-R scoring -- not a floor on the final activation
// score. Real cosine similarities cluster ~0.1-0.4, so a floor above ~0.1
// silently empties the candidate set -> zero results. semantic (0.3) and recent
// (0.2) used to over-filter to zero; they are lowered to 0.1 to match deep and
// the engine default. Mode precision now comes from hops + weights (semantic =
// pure-vector, 0 hops, ACT-R off; recent = recency-biased additive, ACT-R off,
// 1 hop; deep = 4 hops, ACT-R on), NOT from a high similarity floor.
// NOTE: recent disables ACT-R because a PARTIAL weight override + ACT-R silently
// zeroes results (see the recent preset below); the additive path handles its
// partial override correctly, same as semantic.
var recallModePresets = map[string]RecallModePreset{
	"semantic": {
		SemanticSimilarity: 0.8,
		FullTextRelevance:  0.2,
		MaxHops:            0,
		Threshold:          0.1,
		DisableACTR:        true,
	},
	"recent": {
		Recency:            0.7,
		SemanticSimilarity: 0.3,
		MaxHops:            1,
		Threshold:          0.1,
		// DisableACTR routes recent through the additive weighted-sum path (same as
		// semantic, which is GREEN), NOT ACT-R. A PARTIAL weight override (the MCP
		// handler only wires SemanticSimilarity/FullTextRelevance/Recency of the 6
		// scoring dims -> the other 4 default to 0) COMBINED WITH ACT-R silently
		// scored every candidate out -> zero results (false-zeroing, incl. today's
		// memories). recent was the ONLY mode pairing a partial override with ACT-R
		// (semantic disables ACT-R; balanced/deep pass no override -> full defaults).
		// In the additive path the Recency=0.7 weight gives strong, non-zero,
		// recency-biased scoring. (Latent engine bug -- partial override + ACT-R
		// zeroes -- tracked for hardening + Vor's silent-zero canary.)
		DisableACTR: true,
	},
	"balanced": {}, // zero value = engine defaults
	"deep": {
		MaxHops:   4,
		Threshold: 0.1,
	},
}

// LookupRecallMode returns the RecallModePreset for the given name.
// Returns an error for unknown mode names.
func LookupRecallMode(name string) (RecallModePreset, error) {
	m, ok := recallModePresets[name]
	if !ok {
		return RecallModePreset{}, fmt.Errorf("unknown recall mode %q: valid modes are semantic, recent, balanced, deep", name)
	}
	return m, nil
}
