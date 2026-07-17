#!/usr/bin/env bash
# Evolve-graph fixture: prove, against a LIVE muninndb over its real REST API,
# that evolving an engram carries its graph and its epistemic state forward.
#
# This exists because a unit test proves the function; it does not prove the
# system. This drives the same path a real client drives: HTTP -> handler ->
# engine -> Pebble.
#
# THE SCENARIO
#   subject     (confidence 0.5)  -- the claim we are about to revise
#   evidence                      -- subject SUPPORTS evidence      (outbound)
#   objection                     -- objection CONTRADICTS subject   (inbound)
#
#   Evolve subject -> subject'. All three must hold:
#     1. subject' still SUPPORTS evidence          (outbound edge migrated)
#     2. objection still CONTRADICTS subject'      (inbound edge re-parented)
#     3. subject'.confidence == 0.5                (not manufactured to 1.0)
#
#   (2) is the one that matters most: if a contradiction is stranded on the
#   soft-deleted predecessor, a revised claim comes back looking unchallenged,
#   at maximum confidence, with the objection invisible.
#
# EXIT CODES:  0 = all assertions held.  1 = at least one failed.
# The script is expected to FAIL against a pre-fix build. That failure IS the
# reproduction; a fixture that cannot fail proves nothing.
#
# Usage:
#   MUNINN_IMAGE=muninndb:prefix  ./run_fixture.sh     # expect FAIL (repro)
#   MUNINN_IMAGE=muninndb:postfix ./run_fixture.sh     # expect PASS (fix)

set -uo pipefail

cd "$(dirname "$0")"

IMAGE="${MUNINN_IMAGE:-muninndb:latest}"
PORT="${FIXTURE_REST_PORT:-18475}"
API="http://127.0.0.1:${PORT}/api"
# The fixture DB is disposable (anonymous volume, torn down on exit), so the
# unconfigured "default" vault is the right home: non-default vaults require an
# API key, and minting one would add a moving part that proves nothing here.
VAULT="default"
COMPOSE="docker compose -f docker-compose.fixture.yml"

REL_SUPPORTS=1
REL_CONTRADICTS=2

export MUNINN_IMAGE="$IMAGE"
export FIXTURE_REST_PORT="$PORT"

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m  %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILURES=$((FAILURES+1)); }
FAILURES=0

cleanup() { $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

say "Fixture image: ${IMAGE}"
cleanup
$COMPOSE up -d >/dev/null 2>&1 || { echo "compose up failed"; exit 1; }

# ── wait for health ──────────────────────────────────────────────────────────
for i in $(seq 1 40); do
  if curl -sf "${API}/health" >/dev/null 2>&1; then break; fi
  sleep 1
done
if ! curl -sf "${API}/health" >/dev/null 2>&1; then
  echo "server never became healthy; logs:"; $COMPOSE logs --tail=30; exit 1
fi
pass "server healthy on :${PORT}"

# ── helpers ──────────────────────────────────────────────────────────────────
# mk <concept> <content> [confidence] -> prints engram id
mk() {
  local concept="$1" content="$2" conf="${3:-}"
  local body
  if [ -n "$conf" ]; then
    body=$(printf '{"concept":%s,"content":%s,"vault":"%s","confidence":%s}' \
      "$(printf '%s' "$concept" | jq -R .)" "$(printf '%s' "$content" | jq -R .)" "$VAULT" "$conf")
  else
    body=$(printf '{"concept":%s,"content":%s,"vault":"%s"}' \
      "$(printf '%s' "$concept" | jq -R .)" "$(printf '%s' "$content" | jq -R .)" "$VAULT")
  fi
  curl -sf -X POST "${API}/engrams" -H 'Content-Type: application/json' -d "$body" | jq -r '.id'
}

link() { # link <src> <dst> <reltype>
  curl -sf -X POST "${API}/link" -H 'Content-Type: application/json' \
    -d "$(printf '{"source_id":"%s","target_id":"%s","rel_type":%s,"weight":0.75,"vault":"%s"}' "$1" "$2" "$3" "$VAULT")" >/dev/null
}

links_of() { curl -sf "${API}/engrams/$1/links?vault=${VAULT}"; }
engram()   { curl -sf "${API}/engrams/$1?vault=${VAULT}"; }

# ── seed ─────────────────────────────────────────────────────────────────────
say "Seeding memories"
SUBJECT=$(mk "Subject claim" "The claim under revision. Held at 0.5 because we are not sure." "0.5")
EVIDENCE=$(mk "Supporting evidence" "A fact the subject claim leans on.")
OBJECTION=$(mk "The objection" "The reason to doubt the subject claim.")

for v in SUBJECT EVIDENCE OBJECTION; do
  if [ -z "${!v}" ] || [ "${!v}" = "null" ]; then echo "seed failed: $v empty"; exit 1; fi
done
pass "seeded subject=${SUBJECT:0:8}… evidence=${EVIDENCE:0:8}… objection=${OBJECTION:0:8}…"

link "$SUBJECT"   "$EVIDENCE" "$REL_SUPPORTS"      # outbound from subject
link "$OBJECTION" "$SUBJECT"  "$REL_CONTRADICTS"   # inbound  to  subject
pass "linked: subject -supports-> evidence, objection -contradicts-> subject"

# sanity: the pre-evolve graph is really there (else we prove nothing)
PRE=$(links_of "$SUBJECT")
if echo "$PRE" | jq -e --arg t "$EVIDENCE" '[.. | objects | select((.target_id // .TargetID // "") == $t)] | length > 0' >/dev/null 2>&1; then
  pass "pre-evolve: subject's outbound edge is present"
else
  fail "pre-evolve: subject's outbound edge MISSING — fixture is not proving anything"
  echo "$PRE" | head -c 400; echo
fi

CONF_BEFORE=$(engram "$SUBJECT" | jq -r '.confidence // .Confidence // "?"')
pass "pre-evolve: subject confidence = ${CONF_BEFORE}"

# ── evolve ───────────────────────────────────────────────────────────────────
say "Evolving the subject"
NEW=$(curl -sf -X POST "${API}/engrams/${SUBJECT}/evolve?vault=${VAULT}" \
  -H 'Content-Type: application/json' \
  -d '{"new_content":"The revised claim. Still not sure.","reason":"fixture: revision under doubt"}' \
  | jq -r '.id')
if [ -z "$NEW" ] || [ "$NEW" = "null" ]; then echo "evolve failed"; $COMPOSE logs --tail=20; exit 1; fi
pass "evolved: ${SUBJECT:0:8}… -> ${NEW:0:8}…"

# ── assertions ───────────────────────────────────────────────────────────────
say "Assertions"

POST=$(links_of "$NEW")

# 1. outbound edge migrated
if echo "$POST" | jq -e --arg t "$EVIDENCE" '[.. | objects | select((.target_id // .TargetID // "") == $t)] | length > 0' >/dev/null 2>&1; then
  pass "outbound: subject' still SUPPORTS evidence"
else
  fail "outbound: subject' LOST its supports->evidence edge"
fi

# 2. inbound edge re-parented — the objection must still reach the new version
OBJ_LINKS=$(links_of "$OBJECTION")
if echo "$OBJ_LINKS" | jq -e --arg t "$NEW" '[.. | objects | select((.target_id // .TargetID // "") == $t)] | length > 0' >/dev/null 2>&1; then
  pass "inbound: objection still CONTRADICTS subject' (dissent survived)"
else
  fail "inbound: objection still points at the SOFT-DELETED predecessor — subject' looks unchallenged"
fi

# 3. confidence inherited, not manufactured.
#
# The invariant is INHERITANCE -- successor.confidence == predecessor.confidence
# -- NOT any particular number. An earlier version of this fixture asserted
# "== 0.5" and that was a latent false-failure: the engine applies a
# CONTRADICTION PENALTY to a challenged claim (a startup coherence pass drops a
# contradicted engram's confidence hard, e.g. 0.5 -> 0.04). Our subject is
# deliberately contradicted, so once that pass has run the correct inherited
# value is the PENALISED one, and a fixture demanding 0.5 would fail a
# CORRECT system. Compare against what the parent actually held at evolve time.
#
# This is also the sharpest statement of the bug: pre-fix, a claim the engine
# had already discredited to 0.12 evolved back to 1.0 with its accuser
# detached -- the penalty erased AND made un-re-derivable.
CONF_AFTER=$(engram "$NEW" | jq -r '.confidence // .Confidence // "?"')
if awk -v a="$CONF_AFTER" -v b="$CONF_BEFORE" 'BEGIN{d=a-b; if(d<0) d=-d; exit !(d < 0.001)}' 2>/dev/null; then
  pass "confidence: inherited ${CONF_AFTER} (parent held ${CONF_BEFORE})"
else
  fail "confidence: parent held ${CONF_BEFORE}, successor got ${CONF_AFTER} — evolve did not inherit"
fi

say "Result"
if [ "$FAILURES" -eq 0 ]; then
  printf '\033[32mALL ASSERTIONS HELD\033[0m (image: %s)\n' "$IMAGE"; exit 0
else
  printf '\033[31m%d ASSERTION(S) FAILED\033[0m (image: %s)\n' "$FAILURES" "$IMAGE"; exit 1
fi
