package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// TestCountEngramEntities verifies the 0x20 forward-index count.
func TestCountEngramEntities(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	withEntities, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "entity count fixture",
		Content: "Alice and Bob are colleagues.",
		Entities: []mbp.InlineEntity{
			{Name: "Alice", Type: "person"},
			{Name: "Bob", Type: "person"},
		},
	})
	require.NoError(t, err)

	without, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "no entity fixture",
		Content: "Nothing to see here.",
	})
	require.NoError(t, err)

	id, err := storage.ParseULID(withEntities.ID)
	require.NoError(t, err)
	n, err := eng.CountEngramEntities(ctx, "default", id)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	id2, err := storage.ParseULID(without.ID)
	require.NoError(t, err)
	n2, err := eng.CountEngramEntities(ctx, "default", id2)
	require.NoError(t, err)
	require.Equal(t, 0, n2)
}

// TestCountEngramEntities_IsPassive proves the count fires no access feedback:
// scan consumers (the winnow janitor) must never warm the engrams they inspect,
// or the cold-signal they classify against is destroyed by the measurement.
func TestCountEngramEntities_IsPassive(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "passivity fixture",
		Content: "Cold engram that must stay cold.",
		Entities: []mbp.InlineEntity{
			{Name: "ColdCo", Type: "organization"},
		},
	})
	require.NoError(t, err)

	id, err := storage.ParseULID(w.ID)
	require.NoError(t, err)

	before, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "default", ID: w.ID})
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		_, err := eng.CountEngramEntities(ctx, "default", id)
		require.NoError(t, err)
	}

	after, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "default", ID: w.ID})
	require.NoError(t, err)
	// The two Reads themselves may schedule async feedback; the counts must not
	// have moved by the 5 CountEngramEntities calls in between. Access feedback
	// is async fire-and-forget, so equality here (rather than a +2 tolerance)
	// holds because feedback from Read lands via the scoring worker, not the
	// engram record — and CountEngramEntities must contribute exactly nothing.
	require.Equal(t, before.AccessCount, after.AccessCount,
		"CountEngramEntities must not bump AccessCount")
}
