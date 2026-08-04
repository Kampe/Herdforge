package harvest

import "context"

// UnmergedFor is the ONE authoritative single-target "does this worktree
// have genuinely unmerged work" check (port of bin/herd-unmerged). Uses
// git cherry (patch-equivalence against origin/main), never rev-list
// --count.
//
// Why: this repo merges via rebase, replaying each commit onto main with a
// NEW sha; rev-list --count origin/main..branch only compares reachability,
// so an already-merged commit still counts "ahead" — a coordinator reap
// directive built on rev-list held and re-churned already-merged branches
// (2026-07-21 incident). git cherry compares patch content, so it correctly
// reports 0 unique commits once the patch is upstream, however it got there.
//
// Semantics: rev-parse failure → error (not a worktree); main/master/HEAD →
// (nil, nil); transient fetch failure ignored; git cherry failure → (nil,
// nil) matching the zsh `|| true`; no unique commits → (nil, nil).
func (h *Harvester) UnmergedFor(ctx context.Context, worktreePath string) (*UnmergedWork, error) {
	return h.checkUnmergedMode(ctx, worktreePath, false, true)
}

// UnmergedForStrict is the fail-closed variant for destructive callers. It
// uses the same git cherry implementation as UnmergedFor, but reports fetch
// or cherry failures instead of treating them as no unique work.
func (h *Harvester) UnmergedForStrict(ctx context.Context, worktreePath string) (*UnmergedWork, error) {
	return h.checkUnmergedMode(ctx, worktreePath, true, true)
}

// UnmergedForStrictLocal performs strict cherry validation against the
// worktree's existing origin/main ref. Destructive callers that own the fetch
// order use this after their best-effort direct fetch, so a fetch outage does
// not erase usable local evidence while malformed or incomplete cherry
// evidence still fails closed.
func (h *Harvester) UnmergedForStrictLocal(ctx context.Context, worktreePath string) (*UnmergedWork, error) {
	return h.checkUnmergedMode(ctx, worktreePath, true, false)
}
