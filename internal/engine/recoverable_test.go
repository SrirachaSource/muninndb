package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRecoverableUntil_DerivesFromDeletionNotNow pins the defect that made
// recoverable_until useless on the MCP path: it was computed as time.Now() + the
// window, so the deadline RE-BASED on every call. An engram one hour from the limit
// reported a full seven days remaining, and polling never showed the number move --
// a constant wearing a deadline's clothes. The REST path had always derived it from
// deletedAt; both now share this function so they cannot drift again.
func TestRecoverableUntil_DerivesFromDeletionNotNow(t *testing.T) {
	// A deletion firmly in the past: if the result were now-based it would land
	// a week in the FUTURE instead of a week after this timestamp.
	deletedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	got := RecoverableUntil(deletedAt)

	assert.Equal(t, deletedAt.Add(7*24*time.Hour), got,
		"recoverable_until must be deleted_at plus the window")
	assert.True(t, got.Before(time.Now()),
		"a deletion from the past must yield a deadline in the PAST; a future value means it re-based on now")
}

// TestRecoverableUntil_StableAcrossCalls is the observable signature of the bug:
// the same engram must get the same answer every time it is asked. The old
// now-based expression returned a later value on each call, so a caller could not
// tell a fresh deletion from one about to expire.
func TestRecoverableUntil_StableAcrossCalls(t *testing.T) {
	deletedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	first := RecoverableUntil(deletedAt)
	time.Sleep(2 * time.Millisecond) // any now-dependence shows up as drift
	second := RecoverableUntil(deletedAt)

	assert.Equal(t, first, second,
		"the deadline for a given deletion must not move between calls")
}

// TestRecoverableWindow_IsTheAdvertisedSevenDays pins the constant itself. Both
// transports and every SDK type document a seven-day recovery window; if this
// changes, that is an API-visible decision and the SDKs/openapi.yaml must move with
// it, not silently disagree.
func TestRecoverableWindow_IsTheAdvertisedSevenDays(t *testing.T) {
	assert.Equal(t, 7*24*time.Hour, RecoverableWindow)
	assert.Equal(t, int64(604800), int64(RecoverableWindow.Seconds()),
		"604800s is the value the REST fixtures and SDK tests encode")
}
