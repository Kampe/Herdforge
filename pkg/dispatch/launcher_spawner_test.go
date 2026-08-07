package dispatch

import (
	"fmt"
	"testing"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/security"
)

func TestLauncherSpawner_RequireLiveIdentitySessionOptional(t *testing.T) {
	s := newLauncherSpawner(&fakeHerdr{available: true}, launch.Request{})
	// Grok-shaped live entry: no agent_session.
	s.liveLookup = func(name string) (*security.LiveAgentIdentity, error) {
		if name != "task-x" {
			return nil, fmt.Errorf("%w: %s", security.ErrAgentNotFound, name)
		}
		return &security.LiveAgentIdentity{
			Name: "task-x", TabID: "wF:t1", PaneID: "wF:p1", Kind: "grok",
			// AgentSessionID empty
		}, nil
	}
	live, err := s.requireLiveIdentity("task-x", "", "wF:t1", "wF:p1")
	if err != nil || live == nil {
		t.Fatalf("session-less live identity must succeed: %v", err)
	}
	bid, err := bindingID(live)
	if err != nil {
		t.Fatal(err)
	}
	if bid != "live-agent:task-x|wF:t1|wF:p1" {
		t.Fatalf("binding=%s", bid)
	}
	if err := security.RefuseProvisionalWorkerSession(bid); err != nil {
		t.Fatalf("live-agent binding must be accepted: %v", err)
	}
}

func TestLauncherSpawner_RequireLiveIdentityDetectsSessionDrift(t *testing.T) {
	s := newLauncherSpawner(&fakeHerdr{available: true}, launch.Request{})
	s.liveLookup = func(name string) (*security.LiveAgentIdentity, error) {
		return &security.LiveAgentIdentity{
			Name: name, TabID: "t1", PaneID: "p1", AgentSessionID: "ses_live_B",
		}, nil
	}
	if _, err := s.requireLiveIdentity("task-x", "ses_cached_A", "t1", "p1"); err == nil {
		t.Fatal("want session drift error")
	}
	if _, err := s.requireLiveIdentity("task-x", "ses_live_B", "t1", "p1"); err != nil {
		t.Fatal(err)
	}
}

func TestLauncherSpawner_LookupHardErrorFailsClosed(t *testing.T) {
	s := newLauncherSpawner(&fakeHerdr{available: true}, launch.Request{})
	s.started["task-x"] = &security.LiveAgentIdentity{Name: "task-x", AgentSessionID: "should-not-use"}
	s.liveLookup = func(name string) (*security.LiveAgentIdentity, error) {
		return nil, fmt.Errorf("herdr daemon unreachable")
	}
	if _, err := s.Lookup("task-x"); err == nil {
		t.Fatal("hard live error must not fall through to cache")
	}
}

func TestRefuseProvisional_RejectsSesSpawn(t *testing.T) {
	if err := security.RefuseProvisionalWorkerSession("ses_spawn_wK_t9_wK_p9"); err == nil {
		t.Fatal("ses_spawn_ must be refused")
	}
}

// TestLauncherSpawner_ProvisionalWantTreatedAsSessionLess: if spawn seeded a
// ses_spawn_* id, the production path must not pass it as wantSession into
// requireLiveIdentity (that would fail closed on grok's empty live session).
func TestLauncherSpawner_ProvisionalWantTreatedAsSessionLess(t *testing.T) {
	s := newLauncherSpawner(&fakeHerdr{available: true}, launch.Request{})
	s.liveLookup = func(name string) (*security.LiveAgentIdentity, error) {
		return &security.LiveAgentIdentity{
			Name: name, TabID: "wK:t9", PaneID: "wK:p9", Kind: "grok",
		}, nil
	}
	// Simulate the dispatch fix: provisional want → empty want.
	wantSess := "ses_spawn_wK_t9_wK_p9"
	if err := security.RefuseProvisionalWorkerSession(wantSess); err != nil {
		wantSess = ""
	}
	if wantSess != "" {
		t.Fatal("provisional spawn id must clear to session-less want")
	}
	live, err := s.requireLiveIdentity("task-fac-x", wantSess, "wK:t9", "wK:p9")
	if err != nil || live == nil {
		t.Fatalf("session-less want against grok live entry must succeed: %v", err)
	}
	bid, err := bindingID(live)
	if err != nil {
		t.Fatal(err)
	}
	if bid != "live-agent:task-fac-x|wK:t9|wK:p9" {
		t.Fatalf("binding=%s", bid)
	}
}
