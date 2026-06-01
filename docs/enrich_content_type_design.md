# Content-Type-Aware Enrichment — Design Plan

> **Status:** proposed (chair review). Author: Muninn desk, 2026-05-31.
> **Scope:** make MuninnDB enrichment handle JSON and code *as JSON and code*,
> without breaking the prompt cache. Pairs with the `max_tokens=1024` cap fix.

## The gap (verified in source)

The enrichment system prompt is **content-type-blind**. `unifiedPromptHeader`
(`internal/plugin/enrich/prompts.go:12`) opens, verbatim:

> "You are a memory enrichment system. Given a memory's concept and content,
> produce a SINGLE JSON object containing all of the requested fields below."

Nothing in the header or the stage blocks tells the model that:
- a **JSON block** has keys / object names worth naming, or
- a **code block** has a function with a *name*, a *purpose*, and (in a diff) a
  *change* to record.

So the chair is right: Haiku won't do it unless we ask. It follows the prompt.

## The hard constraint (also verified in source)

The system prompt is deliberately kept **byte-stable** so it hits Anthropic's
prompt cache — 90% input discount. The code says so directly:
- `prompts.go:8` — "Kept identical across all engrams so it can be cached"
- `prompts.go:20` — "Each block is identical bytes across every engram"
- `prompts.go:61` — "byte-stable for a given set of stages so repeated calls hit the cache"

⇒ **Branching the system prompt by content type would shatter the cache** and
quietly raise the bill — partly undoing the batch savings just shipped.

## The clean insight

Both code paths build the **per-engram user message** the SAME way — and today
that construction is **duplicated inline** (no shared helper yet):

| path | call site | current code |
|------|-----------|--------------|
| sync | `Run` — `pipeline.go:153` | `user := fmt.Sprintf("Concept: %s\n\nContent: %s", eng.Concept, eng.Content)` |
| batch | `EnrichBatch` — `pipeline.go:237` | `User: fmt.Sprintf("Concept: %s\n\nContent: %s", eng.Concept, eng.Content)` |

The user message is per-engram anyway (never cached). So the plan is: **extract a
`buildUserContent(eng)` helper** from those two identical inline sites, inject the
detected content-type hint inside it, and call it from both. That gives us one
chokepoint, both paths covered, the cached system prompt untouched — and it
removes the existing duplication as a free DRY cleanup.

## Design

**1. New file `internal/plugin/enrich/content_type.go`**

```go
type ContentType int
const ( ContentPlain ContentType = iota; ContentJSON; ContentCode; ContentMixed )

func DetectContentType(content string) ContentType  // cheap, no LLM
func contentHint(t ContentType) string              // per-type guidance line(s)
```

Detection heuristics (string-level, fast):
- **JSON** — `json.Valid()` on the trimmed body, or strong leading `{`/`[` with
  balanced braces + `"key":` density.
- **Code** — fenced ```` ``` ````blocks, signatures (`func `/`def `/`function `/
  `class `), or diff markers (`@@`, leading `+`/`-`).
- **Mixed** — prose that *contains* a fenced code/JSON block (markdown memory).
- **Plain** — none of the above.

**2. The hints (ride in the user message, not the system prompt)**
- JSON → "This engram contains a JSON object. Treat top-level keys and notable
  field names as entities; in the summary describe what the structure represents
  and call out the key fields."
- Code → "This engram contains source code or a diff. Identify the
  function/method/class name(s) as entities; in the summary say what the code
  does and — if a change is described — how it changed (added/modified/removed)."
- Mixed → both, briefly.
- **Plain → "" (empty).** The prose path stays byte-identical to today.

**3. Extract `buildUserContent`, inject the hint, call from both sites**

New helper in `pipeline.go`, replacing the two duplicated inline `fmt.Sprintf`
sites at `:153` (sync `Run`) and `:237` (`EnrichBatch`):

```go
func buildUserContent(eng *storage.Engram) string {
    var b strings.Builder
    if hint := contentHint(DetectContentType(eng.Content)); hint != "" {
        b.WriteString(hint)
        b.WriteString("\n\n")
    }
    fmt.Fprintf(&b, "Concept: %s\n\nContent: %s", eng.Concept, eng.Content)
    return b.String()
}
```

Then both call sites become `buildUserContent(eng)`. One chokepoint, both paths,
cache untouched, duplication removed.

**4. Entity-type homes — no schema change for MVP**

`knownEntityTypes` (`parse.go:14`) already has: project, tool, framework,
language, database, service, technology, concept. Code/data concepts have homes
today — fold function names under `tool`/`concept`, JSON keys under
`concept`/`field`. *Optional fast-follow:* add `function`/`field` types — but
that must update BOTH `parse.go` AND `web/static/js/app.js:getEntityTypeColor`
(the code comment mandates extending them together). Defer unless recall demands.

## Pairs with the truncation cap (the (a) bug)

Content-aware enrichment produces **more** structured output on exactly the
code/JSON engrams that already overflow `max_tokens=1024` →
`ParseUnifiedResponse` fails → `DigestEnrichFailed`. So land the cap-raise in the
same change: thread the existing `LLMProviderConfig.MaxTokens` (`enrich.go:30`,
already defined + defaulted) through the request builders at `anthropic.go:106`
and `anthropic_batch.go:127`, and raise the default (2048 proposed).
`max_tokens` is a *ceiling, not a charge* — raising it costs nothing on normal
memories. One coherent "enrichment quality" change.

## Tests (mandatory)

- `content_type_test.go` — table-driven `DetectContentType`: prose, pure JSON,
  code fence, function signature, unified diff, markdown-with-code (mixed),
  empty, whitespace.
- `contentHint` — assert `ContentPlain → ""` (regression guard: prose path
  unchanged, no cache/behaviour drift on the dominant case).
- `buildUserContent` — hint present for json/code, absent for plain; Concept
  prefix still correct.
- Integration (mock provider) — a JSON engram + a code engram: assert the user
  message carries the hint AND the system prompt is still byte-stable (cache
  guard).

## Rollout

1. Branch `feat/enrich-content-type` off `fork/develop` (SrirachaSource).
2. Docker `vet` + `test` (`golang:1.24-bookworm`) — no local Go.
3. Deploy: `muninndb-cloud` → `fly deploy --no-cache`.
4. Verify: enrich a code + a JSON memory, check `fly logs`, recall the entities.

## Blast radius

Tiny and contained: one new file (`content_type.go`) + extracting a small
`buildUserContent` helper from two duplicated inline sites (a DRY cleanup that
also carries the hint) + (paired) two one-line edits to the request builders + a
default bump. The plain-prose path stays byte-identical to today (hint = ""), and
the **system prompt never changes in any case** — so the 90% cache discount is
fully preserved.

## Open questions for the chair

1. New entity types (`function`/`field`) now, or fold into existing for MVP?
   *(recommend: fold now, add later.)*
2. Default cap: **2048** or 4096? *(2048 covers typical entity-rich memories;
   no cost difference unless output actually grows.)*
3. Ship content-type + cap-raise as **one PR** or two? *(recommend: one —
   thematically and mechanically linked.)*
