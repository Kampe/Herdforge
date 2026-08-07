package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func completeAtt(name, kind, digest, tab, pane, sid, task, lease, role, net, cwd, cont string) SessionAttestation {
	return SessionAttestation{
		Generation: "gen-1", AgentSessionID: sid, TaskRef: task, LeaseGeneration: lease,
		PolicyDigest: digest, AgentName: name, Kind: kind, Role: role, Network: net,
		CWDRel: cwd, Containment: cont, TabID: tab, PaneID: pane,
	}
}

func TestSessionAttestation_StandingReuseGate(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	live := &LiveAgentIdentity{
		Name: "forge-smith", Kind: "opencode", TabID: "w1:t1", PaneID: "w1:p1",
		AgentSessionID: "ses_live_abc",
	}
	if _, err := RequireSessionAttestation(shared, "forge-smith", p, live, "FAC-133", "lease-1"); err == nil {
		t.Fatal("unattested must fail")
	}
	digest, err := ComputePolicyDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	att := completeAtt("forge-smith", "opencode", digest, live.TabID, live.PaneID, live.AgentSessionID,
		"FAC-133", "lease-1", RoleWorker, p.Network, RelIdentity(wt, shared), "sandbox-exec")
	if err := WriteSessionAttestation(shared, att); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireSessionAttestation(shared, "forge-smith", p, live, "FAC-133", "lease-1"); err != nil {
		t.Fatalf("attested reuse: %v", err)
	}
	// Wrong agent_session
	staleSess := *live
	staleSess.AgentSessionID = "ses_other"
	if _, err := RequireSessionAttestation(shared, "forge-smith", p, &staleSess, "FAC-133", "lease-1"); err == nil {
		t.Fatal("wrong agent_session must fail")
	}
	// Wrong tab
	stale := &LiveAgentIdentity{Name: "forge-smith", Kind: "opencode", TabID: "old", PaneID: "old", AgentSessionID: live.AgentSessionID}
	if _, err := RequireSessionAttestation(shared, "forge-smith", p, stale, "FAC-133", "lease-1"); err == nil {
		t.Fatal("stale tab must fail")
	}
	// Wrong lease
	if _, err := RequireSessionAttestation(shared, "forge-smith", p, live, "FAC-133", "lease-OTHER"); err == nil {
		t.Fatal("lease mismatch must fail")
	}
}

func TestSessionAttestation_AtomicWrite(t *testing.T) {
	shared := t.TempDir()
	att := completeAtt("a", "k", "abc", "t", "p", "ses_x", "FAC-1", "lease-1", "worker", "limited", "wt", "sandbox-exec")
	if err := WriteSessionAttestation(shared, att); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(shared, ".herd", "sessions"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".attestation-") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp residue: %s", e.Name())
		}
	}
}

func TestSessionAttestation_IncompleteRejected(t *testing.T) {
	shared := t.TempDir()
	err := WriteSessionAttestation(shared, SessionAttestation{
		Generation: "g", PolicyDigest: "d", AgentName: "a",
	})
	if err == nil {
		t.Fatal("incomplete must fail")
	}
}

func TestNewGeneration_NoFallback(t *testing.T) {
	g, err := NewGeneration()
	if err != nil || g == "" {
		t.Fatalf("gen: %v %s", err, g)
	}
	if !strings.HasPrefix(g, "gen-") {
		t.Fatal(g)
	}
}

func TestEnsureContainedAgent_RetiresStaleStanding(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "e.jsonl")
	_ = BindDurableEvents(p, logPath, &MemorySink{})
	st := StructureTask("FAC-133", "t", "d", RoleWorker, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file"}, Structured: st, ProviderText: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	closed := []string{}
	res := &fakeResolver{
		live: &LiveAgentIdentity{
			Name: "forge-smith", Kind: "true", TabID: "old-tab", PaneID: "old-pane",
			AgentSessionID: "ses_old",
		},
		closed: &closed,
	}
	sp := &recordingSpawner{}
	req := AgentSpawnRequest{
		Policy: p, Grant: grant, Name: "forge-smith", Kind: "true",
		Workspace: "w", EventLogPath: logPath, Ambient: map[string]string{"PATH": "/bin"},
		SkipContainment: true, TaskRef: "FAC-133", LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}},
		SessionResolver: res,
	}
	out, spawn, err := EnsureContainedAgent(res, sp, req, "forge-smith")
	if err != nil {
		t.Fatal(err)
	}
	if out.Reused {
		t.Fatal("must not reuse unattested standing")
	}
	if len(closed) != 1 || closed[0] != "old-tab" {
		t.Fatalf("expected old tab closed, got %v", closed)
	}
	if spawn == nil || spawn.TabID == "" {
		t.Fatal("expected fresh spawn")
	}
	att, err := LoadSessionAttestation(shared, "forge-smith")
	if err != nil {
		t.Fatal(err)
	}
	if att.TabID == "old-tab" {
		t.Fatal("must not bless old tab")
	}
	if att.AgentSessionID == "ses_old" {
		t.Fatal("must not reuse old agent_session")
	}
	if att.TaskRef != "FAC-133" || att.LeaseGeneration != "1" {
		t.Fatalf("task/lease: %+v", att)
	}
}

func TestRetireStanding_CloseErrorFailsClosed(t *testing.T) {
	res := &fakeResolver{
		live:     &LiveAgentIdentity{Name: "a", TabID: "t1", PaneID: "p1", AgentSessionID: "s"},
		closeErr: errCloseBoom,
		closed:   &[]string{},
	}
	if err := RetireStandingAgent(res, "a", "t1"); err == nil {
		t.Fatal("close error must fail")
	}
}

var errCloseBoom = errString("boom")

type errString string

func (e errString) Error() string { return string(e) }

type fakeResolver struct {
	live     *LiveAgentIdentity
	closed   *[]string
	closeErr error
}

func (f *fakeResolver) Lookup(name string) (*LiveAgentIdentity, error) {
	if f.live == nil {
		return nil, ErrAgentNotFound
	}
	return f.live, nil
}
func (f *fakeResolver) CloseTab(tabID string) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	*f.closed = append(*f.closed, tabID)
	f.live = nil
	return nil
}

func TestRequireSessionAttestation_RejectsSkippedContainment(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := ComputePolicyDigest(p)
	if err != nil {
		t.Fatal(err)
	}
	live := &LiveAgentIdentity{Name: "forge-smith", Kind: "claude", TabID: "t1", PaneID: "p1", AgentSessionID: "ses_ok"}
	// Containment "skipped" must never pass the standing reuse gate.
	att := completeAtt("forge-smith", "claude", digest, "t1", "p1", "ses_ok",
		"FAC-133", "1", p.Role, p.Network, RelIdentity(wt, shared), "skipped")
	att.LaunchedAt = time.Now().UTC()
	if err := WriteSessionAttestation(shared, att); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireSessionAttestation(shared, "forge-smith", p, live, "FAC-133", "1"); err == nil {
		t.Fatal("containment=skipped must fail RequireSessionAttestation")
	}
}
