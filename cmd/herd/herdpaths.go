package main

// Shared .herd path fragments and leaf names.
//
// FAC-613: pkg/invariant's duplicate-rule gate caught the same two decisions
// written in two places each -- the managed-worktree path fragment in
// containers_reap.go and main.go, and the harness-hooks pin leaf in
// completion_gate.go and hookspin.go. Both duplications were introduced by
// FAC-598 and FAC-594 respectively.
//
// That is precisely the failure the gate exists to name: the copies drift and a
// fix lands on only one of them. Changing where managed worktrees live, or
// renaming the pin file, previously required remembering an unrelated second
// file.
const (
	// managedWorktreeFrag identifies a path inside the fleet's managed worktree
	// namespace. Kept slash-normalised: callers compare against filepath.ToSlash.
	managedWorktreeFrag = "/.herd/worktrees/"

	// harnessHooksLeaf is the harness hook pin file, always resolved under .herd.
	harnessHooksLeaf = "harness-hooks.json"
)
