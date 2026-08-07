package security

import (
	"time"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// recordingSpawner captures process-boundary CreateTab env for mutation probes.
type recordingSpawner struct {
	cwd       string
	env       []string
	startName string
	startKind string
	startPane string
	startArgs []string
	failTab   error
	failStart error
	closed    []string
}

func (r *recordingSpawner) CreateTab(_, _, cwd string, env []string, _ bool) (string, string, error) {
	if r.failTab != nil {
		return "", "", r.failTab
	}
	r.cwd = cwd
	r.env = append([]string(nil), env...)
	return "tab-1", "pane-1", nil
}
func (r *recordingSpawner) StartAgent(name, kind, paneID string, agentArgs []string) error {
	if r.failStart != nil {
		return r.failStart
	}
	r.startName = name
	r.startKind = kind
	r.startPane = paneID
	r.startArgs = append([]string(nil), agentArgs...)
	return nil
}
func (r *recordingSpawner) CloseTab(tabID string) error {
	r.closed = append(r.closed, tabID)
	return nil
}

func testSpawnPolicy(t *testing.T) (*LaunchPolicy, *LaunchGrant, string) {
	t.Helper()
	wt, shared := testRoots(t)
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	mem := &MemorySink{}
	if err := BindDurableEvents(p, logPath, mem); err != nil {
		t.Fatal(err)
	}
	st := StructureTask("FAC-133", "t", "d", RoleWorker, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file", "git-write"},
		Structured: st, ProviderText: "d", Env: map[string]string{"PATH": "/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, grant, logPath
}

func TestLaunchAgent_ScrubsAmbientSecrets(t *testing.T) {
	p, grant, logPath := testSpawnPolicy(t)
	sp := &recordingSpawner{}
	ambient := map[string]string{
		"PATH":           "/usr/bin",
		"KANEO_API_KEY":  "super-secret",
		"GITHUB_TOKEN":   "ghs_xxx",
		"OPENAI_API_KEY": "sk-xxx",
		"HOME":           "/var/empty/attacker-home",
	}
	res, err := LaunchAgent(sp, AgentSpawnRequest{
		Policy: p, Grant: grant, Name: "task-fac-133", Kind: "opencode",
		Model: "m", Workspace: "w1", Ambient: ambient, EventLogPath: logPath,
		SkipContainment: true, TaskRef: "FAC-133", LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}}, // unit: env scrub only
	})
	if err != nil {
		t.Fatalf("LaunchAgent: %v", err)
	}
	if EnvHasSecret(res.Env, "KANEO_API_KEY", "GITHUB_TOKEN", "OPENAI_API_KEY") {
		t.Fatalf("child env leaked secrets: %v", res.Env)
	}
	if EnvHasSecret(sp.env, "KANEO_API_KEY", "GITHUB_TOKEN", "OPENAI_API_KEY") {
		t.Fatalf("CreateTab env leaked secrets: %v", sp.env)
	}
	joined := strings.Join(sp.env, "\n")
	if strings.Contains(joined, "HOME=/var/empty/attacker-home") {
		t.Fatal("ambient HOME inherited")
	}
	if !strings.Contains(joined, "HOME="+grant.CWD) {
		t.Fatalf("expected HOME=cwd, env=%v", sp.env)
	}
}

func TestLaunchAgent_ReviewerWriteToolBypassDenied(t *testing.T) {
	wt, shared := testRoots(t)
	p, err := PolicyForLane(RoleReviewer, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "e.jsonl")
	if err := BindDurableEvents(p, logPath, &MemorySink{}); err != nil {
		t.Fatal(err)
	}
	st := StructureTask("FAC-133", "t", "d", RoleReviewer, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleReviewer, Tools: []string{"read-file", "git-read"},
		Structured: st, ProviderText: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	grant.AllowedTools = append(grant.AllowedTools, "git-write")
	_, err = LaunchAgent(&recordingSpawner{}, AgentSpawnRequest{
		Policy: p, Grant: grant, Name: "rev", Kind: "opencode", Workspace: "w1",
		Ambient: map[string]string{"PATH": "/bin"}, EventLogPath: logPath,
		SkipContainment: true, TaskRef: "FAC-133", LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}},
	})
	if !errors.Is(err, ErrReviewerWrite) && !errors.Is(err, ErrToolDenied) {
		t.Fatalf("reviewer write bypass must fail: %v", err)
	}
}

func TestLaunchAgent_SiblingCWDDenied(t *testing.T) {
	p, grant, logPath := testSpawnPolicy(t)
	grant.CWD = filepath.Join(filepath.Dir(p.SharedCheckout), "other-repo")
	_, err := LaunchAgent(&recordingSpawner{}, AgentSpawnRequest{
		Policy: p, Grant: grant, Name: "x", Kind: "k", Workspace: "w",
		Ambient: map[string]string{"PATH": "/bin"}, EventLogPath: logPath,
		SkipContainment: true, TaskRef: "FAC-133", LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}},
	})
	if err == nil {
		t.Fatal("sibling cwd must deny")
	}
}

func TestLaunchAgent_DurableEventsPersisted(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "sec.jsonl")
	wt, shared := testRoots(t)
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := BindDurableEvents(p, logPath, &MemorySink{}); err != nil {
		t.Fatal(err)
	}
	st := StructureTask("FAC-133", "t", "d", RoleWorker, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file"}, Structured: st, ProviderText: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = LaunchAgent(&recordingSpawner{}, AgentSpawnRequest{
		Policy: p, Grant: grant, Name: "n", Kind: "k", Workspace: "w", Model: "m",
		Ambient: map[string]string{"PATH": "/bin"}, EventLogPath: logPath,
		SkipContainment: true, TaskRef: "FAC-133", LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sandboxed_agent_started") {
		t.Fatalf("durable log missing launch event: %s", data)
	}
}

func TestLaunchAgent_NilPolicyFailClosed(t *testing.T) {
	_, err := LaunchAgent(&recordingSpawner{}, AgentSpawnRequest{Name: "n", Kind: "k", Workspace: "w", EventLogPath: filepath.Join(t.TempDir(), "e.jsonl"), TaskRef: "FAC-133", LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}}})
	if !errors.Is(err, ErrUnknownPolicy) {
		t.Fatalf("got %v", err)
	}
}

func TestLaunchAgent_StartFailureReturnsPartialTab(t *testing.T) {
	p, grant, logPath := testSpawnPolicy(t)
	sp := &recordingSpawner{failStart: errors.New("boom")}
	res, err := LaunchAgent(sp, AgentSpawnRequest{
		Policy: p, Grant: grant, Name: "n", Kind: "k", Workspace: "w",
		Ambient: map[string]string{"PATH": "/bin"}, EventLogPath: logPath,
		SkipContainment: true, TaskRef: "FAC-133", LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}},
	})
	if err == nil {
		t.Fatal("expected start failure")
	}
	if len(sp.closed) != 1 {
		t.Fatalf("LaunchAgent must close tab on start failure; got %v", sp.closed)
	}
	if res != nil {
		t.Fatalf("expected nil result after start failure cleanup, got %+v", res)
	}
}

func TestLaunchAgent_EmptyEventLogFailClosed(t *testing.T) {
	p, grant, _ := testSpawnPolicy(t)
	_, err := LaunchAgent(&recordingSpawner{}, AgentSpawnRequest{
		Policy: p, Grant: grant, Name: "n", Kind: "k", Workspace: "w",
		Ambient: map[string]string{"PATH": "/bin"}, EventLogPath: "",
		SkipContainment: true, TaskRef: "FAC-133", LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}},
	})
	if !errors.Is(err, ErrUnknownPolicy) {
		t.Fatalf("empty EventLogPath must fail: %v", err)
	}
}

func TestLaunchAgent_ContainmentRequiredOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin")
	}
	p, grant, logPath := testSpawnPolicy(t)
	// Real containment + prove denials + wrapper install.
	// Kind "bash" may not exist as agent — ResolveAgentBinary falls back to kind name.
	// Use /bin/bash as kind by installing wrapper named bash — AgentStart kind is "bash".
	sp := &recordingSpawner{}
	// Force agent binary path via PATH for LookPath of kind "true"
	res, err := LaunchAgent(sp, AgentSpawnRequest{
		Policy: p, Grant: grant, Name: "n", Kind: "true", // /usr/bin/true often exists as "true"
		Workspace: "w1", Ambient: map[string]string{"PATH": "/usr/bin:/bin"},
		EventLogPath: logPath, SkipContainment: false, TaskRef: "FAC-133", LeaseGeneration: "1", ClaimLookup: MapClaimLookup{"FAC-133": {TaskRef: "FAC-133", Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}}, SessionResolver: &stickyResolver{sp: sp},
	})
	if err != nil {
		// true may resolve; if containment probe fails, surface it
		t.Fatalf("LaunchAgent with containment: %v", err)
	}
	if !res.ProvedDenials {
		t.Fatal("expected ProvedDenials")
	}
	if res.Containment != "sandbox-exec" {
		t.Fatalf("containment=%s", res.Containment)
	}
	// PATH must include contain/bin wrapper prefix
	joined := strings.Join(res.Env, "\n")
	if !strings.Contains(joined, ".herd/contain/bin") {
		t.Fatalf("wrapper PATH missing: %v", res.Env)
	}
}

func TestFileEventSink_PropagatesErrors(t *testing.T) {
	// Unwritable path
	s := NewFileEventSink("/dev/null/not-a-dir/events.jsonl")
	err := s.Record(SecurityEvent{Kind: EventDenial, Reason: "x"})
	if err == nil {
		t.Fatal("expected mkdir/write error")
	}
}

func TestConstructAgentEnv_NoProxyBypass(t *testing.T) {
	wt, shared := testRoots(t)
	p, err := PolicyForLane(RoleReviewer, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := StructureTask("FAC-1", "t", "d", RoleReviewer, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleReviewer, Tools: []string{"read-file"}, Structured: st, ProviderText: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	env, err := ConstructAgentEnv(grant, p, map[string]string{"PATH": "/bin", "KANEO_API_KEY": "x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "NO_PROXY=*") {
		t.Fatal("NO_PROXY=* must not be injected (bypasses offline)")
	}
	if strings.Contains(joined, "HTTP_PROXY=") {
		t.Fatal("proxy env must not be used as network control")
	}
	if EnvHasSecret(env, "KANEO_API_KEY") {
		t.Fatal("secret leaked")
	}
}
