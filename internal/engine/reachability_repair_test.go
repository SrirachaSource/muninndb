package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestReachabilityRepair_CleanVaultAndPaging pins the sweep mechanics on a
// healthy vault: every node scanned, none unreachable, and the limit/after
// cursor walks the id space without overlap or loss. (The heal path itself —
// orphan in, reachable out — is proven white-box in
// internal/index/hnsw/inlink_guarantee_test.go TestReinsertRepairsOrphan;
// this level composes proven primitives.)
func TestReachabilityRepair_CleanVaultAndPaging(t *testing.T) {
	eng, _, cleanup := testEnvWithHNSW(t)
	defer cleanup()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:     "repair-test",
			Concept:   fmt.Sprintf("vectored %d", i),
			Content:   fmt.Sprintf("repair sweep specimen %d — distinct content so write-dedup keeps all five", i),
			Embedding: testVec(8, float32(i)),
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// Dry-run, one page, default limit: full census, nothing unreachable,
	// nothing mutated.
	data, err := eng.ReachabilityRepair(ctx, "repair-test", 0, 0, "", true)
	if err != nil {
		t.Fatalf("ReachabilityRepair(dry): %v", err)
	}
	if data.Scanned != 5 || !data.Done {
		t.Errorf("dry census: scanned=%d done=%v, want 5/true", data.Scanned, data.Done)
	}
	if len(data.Unreachable) != 0 || data.Repaired != 0 {
		t.Errorf("healthy vault reported unreachable=%v repaired=%d", data.Unreachable, data.Repaired)
	}
	if !data.DryRun {
		t.Error("DryRun flag not echoed")
	}
	if data.ProbeK != 20 || data.Limit != 500 {
		t.Errorf("defaults: k=%d limit=%d, want 20/500", data.ProbeK, data.Limit)
	}

	// Paged walk at limit=2: pages of 2, 2, 1 with the cursor threading
	// through and Done only on the last.
	var pages []int
	after := ""
	for i := 0; i < 10; i++ {
		page, err := eng.ReachabilityRepair(ctx, "repair-test", 0, 2, after, true)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		pages = append(pages, page.Scanned)
		if page.Done {
			if page.NextAfter != "" {
				t.Errorf("final page carries a cursor: %q", page.NextAfter)
			}
			break
		}
		if page.NextAfter == "" {
			t.Fatalf("page %d not done but no cursor", i)
		}
		after = page.NextAfter
	}
	total := 0
	for _, p := range pages {
		total += p
	}
	if total != 5 || len(pages) != 3 {
		t.Errorf("paged walk: pages=%v (total %d), want [2 2 1] totalling 5", pages, total)
	}

	// Live run on a healthy vault is a no-op that says so.
	data, err = eng.ReachabilityRepair(ctx, "repair-test", 0, 0, "", false)
	if err != nil {
		t.Fatalf("ReachabilityRepair(live): %v", err)
	}
	if data.Repaired != 0 || len(data.Unreachable) != 0 || len(data.Failed) != 0 || len(data.Skipped) != 0 {
		t.Errorf("live run on healthy vault mutated: %+v", data)
	}
}

// TestReachabilityRepair_BadCursorLoud: a malformed after cursor errors at
// parse — never a silent empty page.
func TestReachabilityRepair_BadCursorLoud(t *testing.T) {
	eng, _, cleanup := testEnvWithHNSW(t)
	defer cleanup()

	_, err := eng.ReachabilityRepair(context.Background(), "repair-test", 0, 0, "not-a-ulid", true)
	if err == nil || !strings.Contains(err.Error(), "parse after") {
		t.Fatalf("bad cursor: got err=%v, want parse-after error", err)
	}
}
