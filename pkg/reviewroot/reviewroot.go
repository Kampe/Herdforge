// Package reviewroot resolves the ONE project review root.
//
// FAC-572: the durable handoff mailbox was anchored to the Git common root,
// while the review artifact roots were built cwd-relative. From the shared
// checkout the review inbox held 63 artifacts; from a supervisor worktree the
// same command saw 255 and no outbox at all. So a standing supervisor could
// inspect or ingest a different corpus than the one its queue referred to, with
// nothing in the output saying which.
//
// The location of a project's review corpus is ONE rule. It was resolved two
// ways, which is the same defect class as the mailbox divergence that preceded
// it, and the fix is the same shape: one resolver everything calls.
package reviewroot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// Rel is the review root relative to a repository root.
const Rel = ".herd/review"

// Paths is the resolved review corpus.
type Paths struct {
	// Root is the canonical review root.
	Root string
	// Canonical is false when resolution fell back to a cwd-relative path, so a
	// caller can say so rather than implying authority it does not have.
	Canonical bool
	// Divergent is a non-empty cwd-local review root that is NOT the canonical
	// one, empty when there is none. Reported so an existing corpus is never
	// silently orphaned.
	Divergent string
	// LaneOverride is a HERD_ROOT that names somewhere other than the project
	// root. Surfaced because that override silently redirected the whole
	// resolution before FAC-573.
	LaneOverride string
}

func (p Paths) Inbox() string  { return filepath.Join(p.Root, "inbox") }
func (p Paths) Outbox() string { return filepath.Join(p.Root, "outbox") }
func (p Paths) Ledger() string { return filepath.Join(p.Root, "ledger.jsonl") }
func (p Paths) Queue() string  { return filepath.Join(p.Root, "queue.jsonl") }

// Resolve returns the canonical review root for the project containing startDir.
//
// The anchor is deliberately identical to the handoff mailbox's: the Git common
// root, with HERD_ROOT / HERD_REPO_ROOT as the explicit override. Two
// authorities describing the same review must not disagree about where it lives.
func Resolve(startDir string) Paths {
	if startDir == "" {
		startDir = "."
	}
	p := Paths{}
	// FAC-573: resolve the PROJECT control root, not HERD_ROOT. The launch
	// environment sets HERD_ROOT to the LANE root, so reading it here made a
	// live supervisor resolve a lane-local corpus while five real handoffs sat
	// unread in the project one.
	root, laneOverride, err := gitroot.ProjectRoot(context.Background(), startDir)
	p.LaneOverride = laneOverride
	if err != nil || strings.TrimSpace(root) == "" {
		// Fail SOFT, not closed: refusing to name any review root would stop a
		// supervisor from reading its own corpus. The Canonical flag makes the
		// degraded resolution visible instead of silent.
		p.Root = filepath.Join(startDir, Rel)
		return p
	}
	p.Root = filepath.Join(root, Rel)
	p.Canonical = true
	p.Divergent = divergentLocalRoot(startDir, p.Root)
	return p
}

// divergentLocalRoot reports a cwd-local review root that is not the canonical
// one AND is non-empty.
//
// Emptiness matters: an unused directory is noise, while a populated one is the
// 255-artifact corpus a supervisor would otherwise act on without knowing.
func divergentLocalRoot(startDir, canonical string) string {
	local, err := filepath.Abs(filepath.Join(startDir, Rel))
	if err != nil {
		return ""
	}
	canonicalAbs, err := filepath.Abs(canonical)
	if err != nil || local == canonicalAbs {
		return ""
	}
	entries, err := os.ReadDir(local)
	if err != nil || len(entries) == 0 {
		return ""
	}
	return local
}

// Describe renders the resolution for output, so "which corpus" is answerable
// from what the command printed rather than by re-deriving it.
func (p Paths) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "review root: %s", p.Root)
	if !p.Canonical {
		b.WriteString(" (NOT canonical: resolved relative to the current directory)")
	}
	if p.LaneOverride != "" {
		fmt.Fprintf(&b, "\n  note: %s=%s names a lane, not this project; it is deliberately ignored here",
			gitroot.EnvLaneRoot, p.LaneOverride)
	}
	if p.Divergent != "" {
		fmt.Fprintf(&b, "\n  WARNING: a different, non-empty review root exists at %s;"+
			" artifacts there are NOT part of the corpus above", p.Divergent)
	}
	return b.String()
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
