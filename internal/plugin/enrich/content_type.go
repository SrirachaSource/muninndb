package enrich

import (
	"encoding/json"
	"strings"
)

// ContentType classifies an engram's content so enrichment can hint the model
// to treat structured content (JSON, code) AS structured. The classification
// only ever rides the per-engram USER message (see contentHint /
// buildUserContent) -- never the cached system prompt -- so Anthropic's prompt
// cache (90% input discount) is preserved.
type ContentType int

const (
	// ContentPlain is prose / unstructured text. Its hint is empty, so the
	// user message stays byte-identical to the legacy format.
	ContentPlain ContentType = iota
	// ContentJSON is a body that is (or closely resembles) a JSON value.
	ContentJSON
	// ContentCode is source code or a unified diff.
	ContentCode
	// ContentMixed is prose that embeds a fenced code/JSON block (markdown).
	ContentMixed
)

// DetectContentType classifies content with cheap string-level heuristics (no
// LLM call). Precedence: a body that is wholly JSON -> ContentJSON; a fenced
// block surrounded by prose -> ContentMixed; an all-fence or signature/diff
// body -> ContentCode; a loose-but-bracketed key/value body -> ContentJSON;
// otherwise ContentPlain.
func DetectContentType(content string) ContentType {
	s := strings.TrimSpace(content)
	if s == "" {
		return ContentPlain
	}

	// Whole body is valid JSON -- the most reliable signal, so check it first.
	if (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) && json.Valid([]byte(s)) {
		return ContentJSON
	}

	// Fenced markdown: prose around a fence is Mixed; an all-fence body is Code.
	if strings.Contains(s, "```") {
		if fenceIsWholeBody(s) {
			return ContentCode
		}
		return ContentMixed
	}

	// Unfenced source code or a unified diff.
	if looksLikeCode(s) {
		return ContentCode
	}

	// Bracketed key/value body that strict json.Valid rejected (trailing
	// commas, comments, JSON5) still reads as structured data.
	if looksLikeLooseJSON(s) {
		return ContentJSON
	}

	return ContentPlain
}

// fenceIsWholeBody reports whether s is essentially a single fenced block: it
// opens with a ``` fence and nothing but whitespace follows the closing fence.
func fenceIsWholeBody(s string) bool {
	if !strings.HasPrefix(s, "```") {
		return false
	}
	closing := strings.LastIndex(s, "```")
	if closing == 0 {
		return false // only the opening fence is present
	}
	return strings.TrimSpace(s[closing+3:]) == ""
}

// looksLikeCode reports whether s carries a strong source-code or unified-diff
// signature. The markers are chosen to be rare in ordinary prose; an occasional
// false positive only prepends an advisory hint (low harm) and never changes
// the cached system prompt.
func looksLikeCode(s string) bool {
	if isUnifiedDiff(s) {
		return true
	}
	codeMarkers := []string{
		"func ", "def ", "function ", "class ", "#include",
		"public static", "console.log", "=> {", "});", "<?php",
	}
	for _, m := range codeMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// isUnifiedDiff reports whether s looks like a unified or git diff.
func isUnifiedDiff(s string) bool {
	return strings.Contains(s, "diff --git ") ||
		(strings.Contains(s, "@@ ") && strings.Contains(s, " @@"))
}

// looksLikeLooseJSON reports whether s is bracket-delimited and key-colon dense
// enough to read as structured data even when strict json.Valid fails.
func looksLikeLooseJSON(s string) bool {
	if len(s) < 2 {
		return false
	}
	first, last := s[0], s[len(s)-1]
	bracketed := (first == '{' && last == '}') || (first == '[' && last == ']')
	return bracketed && strings.Contains(s, `":`)
}

// Per-type guidance prepended to the user message. These ride the per-engram
// message only -- never the cached system prompt.
const (
	jsonHint = "This engram contains a JSON object. Treat top-level keys and notable field names as entities; in the summary, describe what the structure represents and call out the key fields."
	codeHint = "This engram contains source code or a diff. Identify the function, method, or class name(s) as entities; in the summary, say what the code does and -- if a change is described -- how it changed (added, modified, or removed)."
)

// contentHint returns the guidance line(s) for a content type. ContentPlain
// returns "" so the prose path (the dominant case) is byte-identical to the
// legacy user message and carries no extra tokens.
func contentHint(t ContentType) string {
	switch t {
	case ContentJSON:
		return jsonHint
	case ContentCode:
		return codeHint
	case ContentMixed:
		return jsonHint + " " + codeHint
	default:
		return ""
	}
}
