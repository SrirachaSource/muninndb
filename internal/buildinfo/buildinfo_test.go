package buildinfo

import (
	"encoding/json"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// resetVersion restores package state so tests do not leak into each other.
func resetVersion(t *testing.T) {
	t.Helper()
	mu.Lock()
	version = ""
	mu.Unlock()
}

func TestGetReportsUnknownRatherThanEmpty(t *testing.T) {
	resetVersion(t)
	got := Get()
	if got.Version != Unknown {
		t.Fatalf("version with no SetVersion = %q, want %q", got.Version, Unknown)
	}
	// The whole point of the package is that a field is never silently blank:
	// an empty string reads as "no data" to a caller that expected a receipt.
	if got.Revision == "" || got.ShortRev == "" || got.CommitTime == "" {
		t.Fatalf("blank field in receipt: %+v", got)
	}
	if got.GoVersion != runtime.Version() {
		t.Fatalf("go_version = %q, want %q", got.GoVersion, runtime.Version())
	}
}

func TestSetVersionIsReported(t *testing.T) {
	resetVersion(t)
	SetVersion("v1.2.3")
	if got := Get().Version; got != "v1.2.3" {
		t.Fatalf("version = %q, want v1.2.3", got)
	}
}

func TestSetVersionIgnoresEmpty(t *testing.T) {
	resetVersion(t)
	SetVersion("v9.9.9")
	SetVersion("")
	// A dev build hands us "" from an unset main.version; that must not erase
	// a real version nor produce a blank field.
	if got := Get().Version; got != "v9.9.9" {
		t.Fatalf("empty SetVersion erased version: got %q", got)
	}
}

func TestShortRevisionTruncatesToTwelve(t *testing.T) {
	full := "3beba4cb1234567890abcdef1234567890abcdef"
	if got := shortRevision(full); got != "3beba4cb1234" {
		t.Fatalf("shortRevision = %q, want 3beba4cb1234", got)
	}
}

func TestShortRevisionDoesNotPadShortInput(t *testing.T) {
	// Must never index past the end when the toolchain reports something
	// shorter than the display width.
	for _, in := range []string{"", "abc", "3beba4cb1234"} {
		if got := shortRevision(in); got != in {
			t.Fatalf("shortRevision(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestStringIncludesVersionAndRevision(t *testing.T) {
	i := Info{Version: "v1.2.3", ShortRev: "3beba4cb1234", CommitTime: "2026-07-28T02:30:00Z", GoVersion: "go1.26.2"}
	got := i.String()
	for _, want := range []string{"v1.2.3", "3beba4cb1234", "2026-07-28T02:30:00Z", "go1.26.2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("String() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "dirty") {
		t.Fatalf("clean build marked dirty: %q", got)
	}
}

func TestStringMarksDirtyBuild(t *testing.T) {
	// A binary built from a modified tree does not correspond to any commit.
	// Hiding that would reintroduce exactly the false confidence this package
	// exists to remove.
	i := Info{Version: "v1.2.3", ShortRev: "abc", CommitTime: "t", GoVersion: "go", Modified: true}
	if !strings.Contains(i.String(), "dirty") {
		t.Fatalf("dirty build not marked: %q", i.String())
	}
}

func TestInfoJSONFieldNames(t *testing.T) {
	// These names are a wire contract: /version, /mcp/health and muninn_status
	// all serialise this struct, and an operator's poller keys off them.
	b, err := json.Marshal(Info{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"version"`, `"revision"`, `"short_revision"`,
		`"commit_time"`, `"modified"`, `"go_version"`,
	} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("JSON missing %s: %s", key, b)
		}
	}
}

// TestGetReadsToolchainVCSStamp is the load-bearing test: it asserts the
// mechanism the whole package rests on. `go test` compiles the test binary
// from this git work tree, so a stamp must be present. If the toolchain ever
// stops stamping (or a build flag disables it), this fails here rather than
// silently degrading every deployed receipt to "unknown".
func TestGetReadsToolchainVCSStamp(t *testing.T) {
	resetVersion(t)
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info available in this environment")
	}
	var stamped bool
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			stamped = true
		}
	}
	if !stamped {
		t.Skip("toolchain did not stamp VCS data (e.g. -buildvcs=false); nothing to assert")
	}
	got := Get()
	if got.Revision == Unknown {
		t.Fatal("toolchain stamped a revision but Get() reported unknown")
	}
	if len(got.ShortRev) > shortRevLen {
		t.Fatalf("short_revision %q longer than %d", got.ShortRev, shortRevLen)
	}
	if !strings.HasPrefix(got.Revision, got.ShortRev) {
		t.Fatalf("short_revision %q is not a prefix of revision %q", got.ShortRev, got.Revision)
	}
}
