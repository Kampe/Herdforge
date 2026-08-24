package main

import "testing"

// FAC-598: the reaper decides by POSITIVE evidence of ephemerality. Anything
// unrecognised is kept, because an unknown long-lived container may be something
// an operator depends on, and a cleanup that guesses is worse than the leak.
func TestReapReasonRequiresPositiveEvidence(t *testing.T) {
	cases := []struct {
		name    string
		c       dockerContainer
		project string
		wantRe  bool
	}{
		{
			name:   "testcontainer is ephemeral by definition",
			c:      dockerContainer{State: "Up 39 hours", Labels: map[string]string{"org.testcontainers": "true"}},
			wantRe: true,
		},
		{
			name:   "testcontainer detected by lang label",
			c:      dockerContainer{State: "Up 2 hours", Labels: map[string]string{"org.testcontainers.lang": "java"}},
			wantRe: true,
		},
		{
			name:   "exited container holds resources but does no work",
			c:      dockerContainer{State: "Exited (255) 3 days ago", Labels: map[string]string{}},
			wantRe: true,
		},
		{
			name: "compose stack rooted in a pool slot is per-run",
			c: dockerContainer{State: "Up 5 hours", Labels: map[string]string{
				"com.docker.compose.project.working_dir": "/Users/k/Personal/chainseer/.herd/pool/pool-03",
			}},
			wantRe: true,
		},
		{
			name: "compose stack rooted in a worktree is per-run",
			c: dockerContainer{State: "Up 5 hours", Labels: map[string]string{
				"com.docker.compose.project.working_dir": "/Users/k/Personal/chainseer/.herd/worktrees/cha-2255",
			}},
			wantRe: true,
		},
		{
			name: "unrecognised long-lived container is KEPT",
			c: dockerContainer{State: "Up 6 days", Labels: map[string]string{
				"com.docker.compose.project.working_dir": "/Users/k/Personal/some-other-app",
			}},
			wantRe: false,
		},
		{
			name:   "bare container with no evidence is KEPT",
			c:      dockerContainer{State: "Up 6 days", Labels: map[string]string{}},
			wantRe: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reapReason(tc.c, tc.project)
			if (got != "") != tc.wantRe {
				t.Errorf("reapReason = %q, wantReapable=%v", got, tc.wantRe)
			}
		})
	}
}

// A shared long-lived stack rooted in the repo itself must never look ephemeral,
// independently of the protected-project allowlist. Two guards are better than
// one when the failure mode is a fleet-wide outage.
func TestEphemeralWorkDirDoesNotMatchTheRepoRoot(t *testing.T) {
	if ephemeralWorkDir("/Users/kampe/Personal/chainseer") {
		t.Error("the repo root is where the shared dev stack lives; it is not ephemeral")
	}
	for _, dir := range []string{
		"/Users/kampe/Personal/chainseer/.herd/pool/pool-01",
		"/Users/kampe/Personal/chainseer/.herd/worktrees/cha-1",
		"/tmp/ci-local-run",
		"/private/tmp/whatever",
		"/Users/kampe/Personal/Herdforge/.worktrees/orchestrator",
	} {
		if !ephemeralWorkDir(dir) {
			t.Errorf("%q is a per-run location and must be reapable", dir)
		}
	}
}
