// Package buildinfo is the single source of truth for what THIS binary is.
//
// Why it exists: a deployed server could not name the source it was built
// from. Operators had to INFER the running revision by comparing a commit
// clock against a build clock — an inference, not a receipt, and one that was
// wrong at least once (a fix believed live on 2026-07-24 was not). The slim
// runtime image carries no git tree and no source, so nothing on the box could
// answer "which ref is this?".
//
// It did not need to be added to the binary — it was already there, unread.
// The Go toolchain stamps VCS data (revision, commit time, dirty flag) into
// every binary built from a version-controlled tree. That stamping survives
// both a `git clone --depth 1` shallow checkout and `-ldflags="-s -w"`, which
// is exactly how the deploy image is built. This package reads that stamp and
// hands it to every surface that should be able to answer the question.
//
// Honesty rule: when the stamp is absent this reports "unknown". It never
// guesses and never infers. A confident wrong receipt is worse than no
// receipt — inference is the failure mode this package exists to retire.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"sync"
)

// Unknown is reported for any field this binary cannot prove.
const Unknown = "unknown"

// shortRevLen is how much of the 40-char sha is kept for display. 12 is the
// git default for an unambiguous short sha.
const shortRevLen = 12

// version is the release version (e.g. "v1.2.3"), injected into package main
// by the release build as -ldflags "-X main.version=..." and handed here by
// SetVersion. It is NOT read from an ldflag directly: main.version is the
// established injection point used by .goreleaser.yml and release.yml, and a
// second injection point for the same field would be a second source of truth.
var (
	mu      sync.RWMutex
	version string
)

// SetVersion records the release version string. Call once at startup from
// package main. Empty input is ignored, so a dev build (where main.version is
// unset) reports Unknown rather than an empty string.
func SetVersion(v string) {
	if v == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	version = v
}

// Info is the build receipt for the running binary.
type Info struct {
	Version   string `json:"version"`        // release version, or "unknown"
	Revision  string `json:"revision"`       // full git sha, or "unknown"
	ShortRev  string `json:"short_revision"` // first 12 of the sha, or "unknown"
	BuildTime string `json:"build_time"`     // RFC3339 commit time, or "unknown"
	Modified  bool   `json:"modified"`       // true = built from a dirty tree
	GoVersion string `json:"go_version"`     // toolchain that compiled it
}

// Get returns the build receipt.
//
// Precedence for the revision is deliberate: the toolchain's VCS stamp is the
// only field the build itself generated from the actual source tree, so it
// cannot disagree with what was compiled. A human-supplied value can. There is
// therefore no override path — this reports what the toolchain observed, or
// Unknown.
func Get() Info {
	mu.RLock()
	v := version
	mu.RUnlock()
	if v == "" {
		v = Unknown
	}

	info := Info{
		Version:   v,
		Revision:  Unknown,
		ShortRev:  Unknown,
		BuildTime: Unknown,
		GoVersion: runtime.Version(),
	}

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" {
				info.Revision = s.Value
				info.ShortRev = shortRevision(s.Value)
			}
		case "vcs.time":
			if s.Value != "" {
				info.BuildTime = s.Value
			}
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	return info
}

// shortRevision truncates a sha for display without ever lengthening a value
// that is already shorter than the display width.
func shortRevision(rev string) string {
	if len(rev) <= shortRevLen {
		return rev
	}
	return rev[:shortRevLen]
}

// String renders the receipt as one operator-readable line.
func (i Info) String() string {
	s := "muninndb " + i.Version + " (" + i.ShortRev + ")"
	if i.Modified {
		s += " [dirty]"
	}
	return s + " built " + i.BuildTime + " with " + i.GoVersion
}
