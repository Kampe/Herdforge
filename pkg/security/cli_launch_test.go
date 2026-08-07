package security

import (
	"time"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type stickyResolver struct {
	sp *recordingSpawner
}

func (s *stickyResolver) Lookup(name string) (*LiveAgentIdentity, error) {
	if s.sp == nil || s.sp.startName == "" {
		return nil, ErrAgentNotFound
	}
	return &LiveAgentIdentity{
		Name: name, Kind: s.sp.startKind, TabID: "tab-1", PaneID: "pane-1",
		AgentSessionID: "ses_test_" + name,
	}, nil
}
func (s *stickyResolver) CloseTab(tabID string) error { return nil }

func TestCLILaunch_WorkerAndReviewerPaths(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("OS containment required")
	}
	sp := &recordingSpawner{}
	for _, role := range []string{RoleWorker, RoleReviewer} {
		t.Run(role, func(t *testing.T) {
			shared := t.TempDir()
			shared, _ = filepath.Abs(shared)
			wt := filepath.Join(shared, "wt-"+role)
			if err := os.MkdirAll(wt, 0o755); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(t.TempDir(), "events.jsonl")
			res, err := (CLILaunch{
				Spawner:        sp,
				ControlSecret:  "secret",
				RepoIdentity:   "herdforge",
				RepoAllowlist:  []string{"herdforge"},
				SharedCheckout: shared,
				Worktree:       wt,
				Role:           role,
				EventLogPath:   logPath,
				TaskRef:        "FAC-133",
				LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}},
				SessionResolver: &stickyResolver{sp: sp},
			}).Run("w1", "agent-"+role, "true", "model-x")
			if err != nil {
				t.Fatalf("CLILaunch.Run: %v", err)
			}
			if !res.ProvedDenials {
				t.Fatal("expected denial proofs")
			}
			if res.AgentSessionID == "" {
				t.Fatal("expected agent_session_id")
			}
			if role == RoleReviewer && res.Network != "limited" {
				// Reviewers use limited (brokered model transport), not full offline.
				t.Fatalf("reviewer network=%s", res.Network)
			}
		})
	}
}
