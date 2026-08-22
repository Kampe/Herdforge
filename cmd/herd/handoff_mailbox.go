package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// canonicalHandoffMailbox resolves the ONE shared handoff bus for this
// repository.
//
// FAC-572: mail.CallbackMailPath(".") is CWD-relative, so a coordinator at the
// repo root and an agent inside a worktree resolve DIFFERENT files. A shared
// fleet bus whose location depends on the caller's directory is split-brain by
// construction: one side queues, the other reads an empty file, and both are
// reporting honestly about different queues.
//
// This is the same defect shape as FAC-565's two review-ledger paths, where
// drain and review-ingest disagreed for exactly this reason. Resolution is
// anchored to the canonical repository root, and HERD_MAIL_FILE still overrides
// when an operator wants an explicit bus.
//
// A divergent worktree-local file is REPORTED rather than ignored: silently
// switching buses could orphan entries someone is waiting on.
func canonicalHandoffMailbox() string {
	root, err := worktree.ResolveCanonicalRoot(context.Background(), ".",
		firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ""))
	if err != nil || root == "" {
		// Fail soft to the previous behaviour rather than refusing to read any
		// queue at all; the disclosed path makes the fallback visible.
		return mail.CallbackMailPath(".")
	}
	canonical := mail.CallbackMailPath(root)
	warnIfDivergentMailbox(canonical)
	return canonical
}

// warnIfDivergentMailbox reports a CWD-local bus that is not the canonical one
// and is non-empty, so entries written under the old resolution are not lost
// without a word.
func warnIfDivergentMailbox(canonical string) {
	local := mail.CallbackMailPath(".")
	localAbs, err1 := filepath.Abs(local)
	canonAbs, err2 := filepath.Abs(canonical)
	if err1 != nil || err2 != nil || localAbs == canonAbs {
		return
	}
	info, statErr := os.Stat(localAbs)
	if statErr != nil || info.Size() == 0 {
		return
	}
	fmt.Fprintf(os.Stderr,
		"herd handoffs: NOTE a non-empty mailbox also exists at %s (%d bytes) but the canonical bus is %s.\n"+
			"  Entries written under the old CWD-relative resolution live in the former and are not listed here.\n",
		localAbs, info.Size(), canonAbs)
}
