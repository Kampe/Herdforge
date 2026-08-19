package dispatch

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// countingProvider records how many inner calls actually happened — the
// mutation-proof for "rejected BEFORE the provider command".
type countingProvider struct {
	mockTaskProvider
	calls int32
}

func (c *countingProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	atomic.AddInt32(&c.calls, 1)
	return c.mockTaskProvider.GetTask(ctx, id)
}
func (c *countingProvider) ListTasks(ctx context.Context, p, s string) ([]*provider.Task, error) {
	atomic.AddInt32(&c.calls, 1)
	return c.mockTaskProvider.ListTasks(ctx, p, s)
}
func (c *countingProvider) ClaimTask(ctx context.Context, id, role string) error {
	atomic.AddInt32(&c.calls, 1)
	return c.mockTaskProvider.ClaimTask(ctx, id, role)
}
func (c *countingProvider) UpdateStatus(ctx context.Context, id, st string) error {
	atomic.AddInt32(&c.calls, 1)
	return c.mockTaskProvider.UpdateStatus(ctx, id, st)
}
func (c *countingProvider) AddComment(ctx context.Context, id, b string) error {
	atomic.AddInt32(&c.calls, 1)
	return c.mockTaskProvider.AddComment(ctx, id, b)
}

// testAuthority is the canonical binding matching validTaskContext.
func testAuthority() BindingAuthority {
	return BindingAuthority{
		Repository:        "herdforge",
		ProviderType:      "kaneo",
		ProjectID:         "proj-x",
		ProviderWorkspace: "ws-1",
		ProviderProfile:   "KANEO_API_KEY",
	}
}

// testSignerVerifier builds an isolated signer (private key in a temp dir)
// and its verifier loaded from the PUBLISHED public key — the exact trust
// path production uses.
// attestedKeyDir simulates an EXTERNAL isolation boundary (what FAC-133's
// sandbox or an OS keychain policy provides in production). Herdforge never
// writes this claim about itself.
func attestedKeyDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := WriteIsolationAttestation(dir, "test-sandbox"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testSignerVerifier(t *testing.T) (*Signer, *Verifier) {
	t.Helper()
	root := t.TempDir()
	// Signing fixtures exercise receipt validation, not the ambient worker
	// boundary. Keep them outside managed worktrees and clear the inherited
	// Herdr agent marker; TestSignerBoundary_AgentRoleEnvRefused covers that
	// boundary explicitly.
	t.Chdir(root)
	t.Setenv("HERD_ROLE", "")
	s, err := LoadOrCreateSigner(attestedKeyDir(t), "herdforge", root)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	v, err := LoadVerifier(root)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return s, v
}

func mustIssue(t *testing.T, s *Signer, tc TaskContext) TaskContext {
	t.Helper()
	signed, err := s.Issue(tc)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return signed
}

// Every rejection class the receipt must enforce, each proven to fail
// BEFORE the inner provider is called (calls stays 0). Receipts are
// legitimately SIGNED — the rejection must come from the targeted check,
// not signature failure. Removing any single check makes its case fail —
// these are the FAC-145 mutation proofs.
func TestContextBoundProvider_FailsClosedBeforeProviderCall(t *testing.T) {
	ctx := context.Background()
	signer, verifier := testSignerVerifier(t)
	now := func() time.Time { return time.Now() }

	cases := []struct {
		name string
		tc   func() TaskContext
		gen  int64
		call func(p *ContextBoundProvider) error
	}{
		{
			name: "expired receipt",
			tc: func() TaskContext {
				tc := validTaskContext()
				tc.ExpiresAt = time.Now().Add(-time.Minute)
				return tc
			},
			call: func(p *ContextBoundProvider) error { _, err := p.GetTask(ctx, "task-id-1"); return err },
		},
		{
			name: "over-privileged op (worker mutate)",
			tc:   validTaskContext,
			call: func(p *ContextBoundProvider) error { return p.UpdateStatus(ctx, "task-id-1", "done") },
		},
		{
			name: "over-privileged op (worker claim)",
			tc:   validTaskContext,
			call: func(p *ContextBoundProvider) error { return p.ClaimTask(ctx, "task-id-1", "worker") },
		},
		{
			name: "wrong task",
			tc:   validTaskContext,
			call: func(p *ContextBoundProvider) error { _, err := p.GetTask(ctx, "other-task"); return err },
		},
		{
			name: "wrong task comment",
			tc:   validTaskContext,
			call: func(p *ContextBoundProvider) error { return p.AddComment(ctx, "other-task", "hi") },
		},
		{
			name: "redirected project list",
			tc:   validTaskContext,
			call: func(p *ContextBoundProvider) error { _, err := p.ListTasks(ctx, "other-project", ""); return err },
		},
		{
			name: "stale lease generation",
			tc:   validTaskContext, // generation 3
			gen:  4,
			call: func(p *ContextBoundProvider) error { _, err := p.ListTasks(ctx, "proj-x", ""); return err },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inner := &countingProvider{}
			p, err := NewContextBoundProvider(inner, mustIssue(t, signer, c.tc()), testAuthority(), verifier, now, c.gen)
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			if err := c.call(p); err == nil {
				t.Fatal("must fail closed")
			}
			if got := atomic.LoadInt32(&inner.calls); got != 0 {
				t.Fatalf("inner provider was reached %d time(s) — rejection must happen BEFORE the provider command", got)
			}
		})
	}

	// Non-vacuity: the same receipt permits its own bounded operations.
	inner := &countingProvider{mockTaskProvider: mockTaskProvider{
		tasks: []*provider.Task{{ID: "task-id-1", Ref: "FAC-145", Status: "to-do"}},
	}}
	p, err := NewContextBoundProvider(inner, mustIssue(t, signer, validTaskContext()), testAuthority(), verifier, now, 3)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := p.GetTask(ctx, "task-id-1"); err != nil {
		t.Fatalf("own task read must pass: %v", err)
	}
	if _, err := p.ListTasks(ctx, "proj-x", ""); err != nil {
		t.Fatalf("bound project list must pass: %v", err)
	}
	if err := p.AddComment(ctx, "task-id-1", "note"); err != nil {
		t.Fatalf("own task comment must pass: %v", err)
	}
	if atomic.LoadInt32(&inner.calls) != 3 {
		t.Fatalf("authorized calls must reach the provider, got %d", inner.calls)
	}

	// Unsigned and invalid receipts can never construct a usable provider,
	// and a nil verifier is refused outright.
	if _, err := NewContextBoundProvider(inner, validTaskContext(), testAuthority(), verifier, now, 0); err == nil {
		t.Fatal("unsigned receipt must not construct")
	}
	bad := validTaskContext()
	bad.ProjectID = ""
	if _, err := NewContextBoundProvider(inner, bad, testAuthority(), verifier, now, 0); err == nil {
		t.Fatal("invalid receipt must not construct")
	}
	if _, err := NewContextBoundProvider(inner, mustIssue(t, signer, validTaskContext()), testAuthority(), nil, now, 0); err == nil {
		t.Fatal("nil verifier must be refused")
	}
}

func TestCheckSignerBoundary_DiagnosesMissingAttestation(t *testing.T) {
	keyDir := t.TempDir()
	repoRoot := t.TempDir()
	t.Setenv(KeyDirEnv, keyDir)
	t.Setenv("HERD_MODE", "production")

	err := CheckSignerBoundary(repoRoot)
	if err == nil {
		t.Fatal("missing attestation must block signer-boundary readiness")
	}
	msg := err.Error()
	want := []string{"attest key not established at", filepath.Join(keyDir, IsolationAttestFile), "herd signer-boundary establish"}
	for _, fragment := range want {
		if !strings.Contains(msg, fragment) {
			t.Fatalf("diagnosis missing %q: %s", fragment, msg)
		}
	}
}

// A receipt that does not match the CANONICAL repository/provider/project/
// workspace/profile authority is rejected at construction — a differently
// focused repo or workspace can never redirect provider traffic (FAC-145
// mutation proofs for BindingAuthority.matches).
func TestContextBoundProvider_AuthorityMismatchRejected(t *testing.T) {
	signer, verifier := testSignerVerifier(t)
	inner := &countingProvider{}
	signed := mustIssue(t, signer, validTaskContext())
	mutations := map[string]func(*BindingAuthority){
		"wrong focused repository": func(a *BindingAuthority) { a.Repository = "other-repo" },
		"provider type mismatch":   func(a *BindingAuthority) { a.ProviderType = "jira" },
		"project mismatch":         func(a *BindingAuthority) { a.ProjectID = "other-project" },
		"workspace mismatch":       func(a *BindingAuthority) { a.ProviderWorkspace = "ws-other" },
		"profile mismatch":         func(a *BindingAuthority) { a.ProviderProfile = "OTHER_KEY_ENV" },
		"incomplete authority":     func(a *BindingAuthority) { a.ProjectID = "" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			a := testAuthority()
			mutate(&a)
			if _, err := NewContextBoundProvider(inner, signed, a, verifier, nil, 0); err == nil {
				t.Fatal("mismatched authority must not construct a provider")
			}
			if got := atomic.LoadInt32(&inner.calls); got != 0 {
				t.Fatalf("inner provider reached %d time(s)", got)
			}
		})
	}
}

// FAC-145: fencing applies to EVERY receipt — one behind a known newer
// generation authorizes nothing.
func TestContextBoundProvider_ReceiptBehindGenerationFenced(t *testing.T) {
	signer, verifier := testSignerVerifier(t)
	inner := &countingProvider{}
	tc := validTaskContext()
	tc.LeaseGeneration = 1
	p, err := NewContextBoundProvider(inner, mustIssue(t, signer, tc), testAuthority(), verifier, nil, 2)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if _, err := p.ListTasks(context.Background(), "proj-x", ""); err == nil {
		t.Fatal("generation-1 receipt behind current generation 2 must be fenced")
	}
	if got := atomic.LoadInt32(&inner.calls); got != 0 {
		t.Fatalf("inner provider reached %d time(s)", got)
	}
}

// Malicious-worker proof: any field edited after coordinator issuance
// breaks verification, field-rewrite widening has no path, and the entire
// repo + worktree tree a worker may read contains NO signing material.
func TestReceiptAuthority_MaliciousWorkerCannotForge(t *testing.T) {
	keyDir := attestedKeyDir(t) // coordinator-private, OUTSIDE the repo tree
	repoRoot := t.TempDir()
	signer, err := LoadOrCreateSigner(keyDir, "herdforge", repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := LoadVerifier(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(repoRoot, ".herd", "worktrees", "fac-145")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatal(err)
	}
	signed := mustIssue(t, signer, validTaskContext())
	if err := WriteTaskContext(worktree, signed); err != nil {
		t.Fatal(err)
	}

	// 1. Tamper matrix: every field edit is detected.
	tampers := map[string]func(*TaskContext){
		"role widening":     func(tc *TaskContext) { tc.Role = RoleCoordinator },
		"ops widening":      func(tc *TaskContext) { tc.AllowedOps = CoordinatorOps },
		"task retargeting":  func(tc *TaskContext) { tc.TaskID = "victim-task" },
		"ref retargeting":   func(tc *TaskContext) { tc.TaskRef = "FAC-999" },
		"project switch":    func(tc *TaskContext) { tc.ProjectID = "other-project" },
		"expiry extension":  func(tc *TaskContext) { tc.ExpiresAt = tc.ExpiresAt.Add(240 * time.Hour) },
		"generation bump":   func(tc *TaskContext) { tc.LeaseGeneration = 99 },
		"repository switch": func(tc *TaskContext) { tc.Repository = "other-repo" },
	}
	for name, tamper := range tampers {
		t.Run(name, func(t *testing.T) {
			forged := signed
			tamper(&forged)
			if err := verifier.Verify(forged); err == nil {
				t.Fatal("tampered receipt must fail verification")
			}
		})
	}

	// 2. The worker-readable tree (repo root, including its worktree and the
	// published public key) contains NO private signing material.
	seedData, err := os.ReadFile(filepath.Join(keyDir, "herdforge.ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimSpace(string(seedData))
	walkErr := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		if strings.Contains(string(data), secret) {
			return fmt.Errorf("private key material leaked into worker-readable path %s", path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}

	// 3. Nothing found in the readable tree lets the worker mint authority:
	// signing with the PUBLIC key (all they have) does not verify.
	pubData, err := os.ReadFile(filepath.Join(repoRoot, ReceiptPubFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(pubData)) == secret {
		t.Fatal("published key must not equal private material")
	}
	forged := signed
	forged.Role = RoleCoordinator
	forged.Signature = signed.Signature // replayed signature over edited body
	if err := verifier.Verify(forged); err == nil {
		t.Fatal("replayed signature over a forged body must fail")
	}

	// 4. Verification with a DIFFERENT published key rejects receipts from
	// this coordinator (foreign authority is not interchangeable).
	otherRoot := t.TempDir()
	if _, err := LoadOrCreateSigner(attestedKeyDir(t), "herdforge", otherRoot); err != nil {
		t.Fatal(err)
	}
	otherVerifier, err := LoadVerifier(otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := otherVerifier.Verify(signed); err == nil {
		t.Fatal("foreign verifier must reject this coordinator's receipts")
	}
}

// IssueCoordinator is the ONLY widening path: it authenticates the source
// receipt first and re-signs the widened context; a receipt tampered before
// widening is refused.
func TestSigner_IssueCoordinator(t *testing.T) {
	signer, verifier := testSignerVerifier(t)
	signed := mustIssue(t, signer, validTaskContext())

	coord, err := signer.IssueCoordinator(signed)
	if err != nil {
		t.Fatalf("widen: %v", err)
	}
	if coord.Role != RoleCoordinator {
		t.Errorf("role = %q", coord.Role)
	}
	if err := verifier.Verify(coord); err != nil {
		t.Fatalf("widened context must verify: %v", err)
	}
	if coord.TaskRef != signed.TaskRef || coord.LeaseID != signed.LeaseID || coord.LeaseGeneration != signed.LeaseGeneration {
		t.Errorf("binding changed: %+v", coord)
	}
	hasMutate := false
	for _, op := range coord.AllowedOps {
		if op == "mutate" {
			hasMutate = true
		}
	}
	if !hasMutate {
		t.Error("coordinator context must allow mutate")
	}

	forged := signed
	forged.TaskID = "victim-task"
	if _, err := signer.IssueCoordinator(forged); err == nil {
		t.Fatal("widening a tampered receipt must be refused")
	}
	if _, err := signer.IssueCoordinator(validTaskContext()); err == nil {
		t.Fatal("widening an unsigned receipt must be refused")
	}
}

// The coordinator-only signing boundary achievable under one Unix user:
// key material inside the repo tree is refused, signing from a managed
// agent worktree cwd is refused, and exposed key permissions are refused.
func TestSignerBoundary(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadOrCreateSigner(filepath.Join(root, ".herd", "keys"), "herdforge", root); err == nil {
		t.Fatal("key dir inside the repository tree must be refused")
	}

	keyDir := t.TempDir()
	wt := filepath.Join(root, ".herd", "worktrees", "fac-1")
	if err := os.MkdirAll(wt, 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wt)
	if _, err := LoadOrCreateSigner(keyDir, "herdforge", root); err == nil {
		t.Fatal("signing from a managed agent worktree must be refused")
	}
}

// The herdr-injected agent role marker refuses signing regardless of cwd:
// every process an agent spawns inherits it.
func TestSignerBoundary_AgentRoleEnvRefused(t *testing.T) {
	keyDir, root := attestedKeyDir(t), t.TempDir()
	t.Setenv("HERD_ROLE", "agent")
	if _, err := LoadOrCreateSigner(keyDir, "herdforge", root); err == nil {
		t.Fatal("agent-role process must be refused signing")
	}
}

func TestSigner_ExposedKeyPermissionsRefused(t *testing.T) {
	keyDir, root := attestedKeyDir(t), t.TempDir()
	if _, err := LoadOrCreateSigner(keyDir, "herdforge", root); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(keyDir, "herdforge.ed25519"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSigner(keyDir, "herdforge", root); err == nil {
		t.Fatal("group/world-readable key material must be refused")
	}
}

// The signer never rotates authority: existing key material wins races and
// corrupt/unreadable state is a hard error, and a mismatched published key
// refuses rather than overwrites.
func TestSigner_NonRotatingKeyLifecycle(t *testing.T) {
	keyDir, root := attestedKeyDir(t), t.TempDir()
	s1, err := LoadOrCreateSigner(keyDir, "herdforge", root)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := LoadOrCreateSigner(keyDir, "herdforge", root)
	if err != nil {
		t.Fatal(err)
	}
	v, err := LoadVerifier(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(mustIssue(t, s2, validTaskContext())); err != nil {
		t.Fatalf("reloaded signer must produce verifiable receipts: %v", err)
	}
	_ = s1

	// Corrupt private key: hard error, never regenerate.
	if err := os.WriteFile(filepath.Join(keyDir, "herdforge.ed25519"), []byte("not-hex"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSigner(keyDir, "herdforge", root); err == nil {
		t.Fatal("corrupt key must be a hard error, not a rotation")
	}

	// Fresh key dir with an EXISTING different published key: refuse.
	if _, err := LoadOrCreateSigner(attestedKeyDir(t), "herdforge", root); err == nil {
		t.Fatal("mismatched published key must refuse rotation")
	}
}

// tripwireTransport fails the moment any adapter opens a network connection.
type tripwireTransport struct{ hits *int32 }

func (tr tripwireTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	atomic.AddInt32(tr.hits, 1)
	return nil, fmt.Errorf("FAC-145 conformance: transport for %s must never be reached", r.URL.Host)
}

// One provider-neutral conformance pass over every production adapter:
// an unauthorized receipt produces ZERO transport traffic on kaneo, github,
// linear, jira, azure, and zero state change on memory.
func TestConformance_ReceiptEnforcement_AllAdapters(t *testing.T) {
	ctx := context.Background()
	signer, verifier := testSignerVerifier(t)
	var hits int32
	client := &http.Client{Transport: tripwireTransport{hits: &hits}}

	kaneo := provider.NewKaneoProvider("http://kaneo.invalid", "proj-x", false)
	kaneo.Client = client
	github := provider.NewGitHubProvider("tok", "owner", "repo")
	github.Client = client
	linear := provider.NewLinearProvider("key")
	linear.Client = client
	jira := provider.NewJiraProvider("http://jira.invalid", "u@example.com", "tok")
	jira.HTTPClient = client
	azure := provider.NewAzureDevOpsProvider("http://azure.invalid/org", "proj", "pat")
	azure.HTTPClient = client
	memory := provider.NewMemoryProvider()
	memory.AddTask(&provider.Task{ID: "task-id-1", Ref: "FAC-145", Status: "to-do", ProjectID: "proj-x"})

	adapters := []struct {
		name string
		tp   provider.TaskProvider
	}{
		{"kaneo", kaneo}, {"github", github}, {"linear", linear},
		{"jira", jira}, {"azure", azure}, {"memory", memory},
	}

	workerReceipt := mustIssue(t, signer, validTaskContext())
	for _, a := range adapters {
		t.Run(a.name, func(t *testing.T) {
			p, err := NewContextBoundProvider(a.tp, workerReceipt, testAuthority(), verifier, nil, 0)
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			if err := p.UpdateStatus(ctx, "task-id-1", "done"); err == nil {
				t.Error("worker receipt must not authorize a status mutation")
			}
			if _, err := p.GetTask(ctx, "other-task"); err == nil {
				t.Error("receipt must not authorize reading another task")
			}
			if _, err := p.ListTasks(ctx, "other-project", ""); err == nil {
				t.Error("receipt must not authorize listing another project")
			}
		})
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("unauthorized ops produced %d transport hit(s); enforcement must fail before the provider command", got)
	}
	if task, _ := memory.GetTask(ctx, "task-id-1"); task == nil || task.Status != "to-do" {
		t.Fatalf("memory adapter state must be untouched, got %+v", task)
	}

	// Non-vacuity: a signer-issued coordinator context performs the same
	// mutation on the memory adapter successfully (and only on its own task).
	coord, err := signer.IssueCoordinator(workerReceipt)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewContextBoundProvider(memory, coord, testAuthority(), verifier, nil, 0)
	if err != nil {
		t.Fatalf("construct coordinator: %v", err)
	}
	if err := p.UpdateStatus(ctx, "task-id-1", "in-review"); err != nil {
		t.Fatalf("coordinator mutation on own task must pass: %v", err)
	}
	if err := p.UpdateStatus(ctx, "some-other", "done"); err == nil {
		t.Fatal("coordinator receipt is still bound to one task")
	}
}

// Worker FAIL and coordinator PASS callbacks derive from the same receipt
// binding and therefore carry the identical repo + lease + task identity.
func TestBoundCallback_IdenticalBindingAcrossRoles(t *testing.T) {
	signer, _ := testSignerVerifier(t)
	receipt := mustIssue(t, signer, validTaskContext())

	workerFail, err := receipt.BoundCallback(mail.CallbackBlocked, "", "tests failed")
	if err != nil {
		t.Fatalf("worker FAIL callback: %v", err)
	}
	coord, err := signer.IssueCoordinator(receipt)
	if err != nil {
		t.Fatal(err)
	}
	coordPass, err := coord.BoundCallback(mail.CallbackComplete, "abc123", "approved")
	if err != nil {
		t.Fatalf("coordinator PASS callback: %v", err)
	}

	if workerFail.Ref != coordPass.Ref || workerFail.Repo != coordPass.Repo ||
		workerFail.LeaseGeneration != coordPass.LeaseGeneration {
		t.Fatalf("bindings differ:\n fail=%+v\n pass=%+v", workerFail, coordPass)
	}
	if workerFail.Ref != "FAC-145" || workerFail.Repo != "herdforge" || workerFail.LeaseGeneration != 3 {
		t.Fatalf("binding not taken from the receipt: %+v", workerFail)
	}
	if workerFail.SenderRole != RoleWorker || coordPass.SenderRole != RoleCoordinator {
		t.Fatalf("sender roles wrong: %q / %q", workerFail.SenderRole, coordPass.SenderRole)
	}

	if _, err := receipt.BoundCallback(mail.CallbackComplete, " ", "done"); err == nil {
		t.Fatal("complete callback without SHA must be refused")
	}
	broken := receipt
	broken.Repository = ""
	if _, err := broken.BoundCallback(mail.CallbackBlocked, "", "x"); err == nil {
		t.Fatal("invalid receipt must not produce a callback")
	}
}

func TestTaskContext_ForRepositoryAndAbsolutePathRejection(t *testing.T) {
	tc := validTaskContext()
	if err := tc.ForRepository("Herdforge"); err != nil {
		t.Fatalf("case-insensitive repo match must pass: %v", err)
	}
	if err := tc.ForRepository("other-repo"); err == nil {
		t.Fatal("cross-repository use must be rejected")
	}

	abs := validTaskContext()
	// Built at runtime so the repo's own preflight boundary scan never sees
	// an absolute-path literal in source.
	abs.HerdrWorkspace = string(filepath.Separator) + filepath.Join("users", "someone", "workspaces", "wF")
	if err := abs.Validate(); err == nil {
		t.Fatal("absolute host path in receipt must be rejected")
	}
}

// FAC-145 (blocker 7): repository identity is derived from the STABLE
// remote, not just the configured name. Two different repositories sharing
// a configured name get different identities, different signing keys, and
// therefore cannot verify each other's receipts.
func TestRepositoryIdentity_SameNameForeignRepoRejected(t *testing.T) {
	mkRepo := func(remote string) string {
		dir := t.TempDir()
		run := func(args ...string) {
			c := exec.Command("git", args...)
			c.Dir = dir
			c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		run("init")
		run("remote", "add", "origin", remote)
		return dir
	}
	repoA := mkRepo("https://github.com/acme/widgets.git")
	repoB := mkRepo("https://github.com/other/widgets.git")

	idA, err := RepositoryIdentity(repoA, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	idB, err := RepositoryIdentity(repoB, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if idA == idB {
		t.Fatalf("same-named foreign repositories must not share identity: %s", idA)
	}
	// ssh and https forms of the SAME repository normalize to one identity.
	repoASSH := mkRepo("git@github.com:acme/widgets.git")
	idASSH, err := RepositoryIdentity(repoASSH, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if idASSH != idA {
		t.Fatalf("ssh/https forms must share identity: %s vs %s", idASSH, idA)
	}

	// Distinct identities mean distinct signing material: A's receipt does
	// not verify under B's published key.
	keyDir := attestedKeyDir(t)
	signerA, err := LoadOrCreateSigner(keyDir, idA, repoA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSigner(keyDir, idB, repoB); err != nil {
		t.Fatal(err)
	}
	verifierB, err := LoadVerifier(repoB)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signerA.Issue(validTaskContext())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifierB.Verify(signed); err == nil {
		t.Fatal("a same-named foreign repository must not verify this repository's receipts")
	}
}

// FAC-145: signing authority is refused when the key store is not
// demonstrably isolated — checked here and now, independent of FAC-133
// landing (FAC-133 becomes the writer of the attestation, not the reason
// the check exists).
func TestSigner_RefusesNonIsolatedKeyStore(t *testing.T) {
	root := t.TempDir()

	// NO attestation: signing is refused outright — an unattested key store
	// is readable by same-uid agents, so it carries no authority.
	bare := t.TempDir()
	if _, err := LoadOrCreateSigner(bare, "herdforge", root); err == nil {
		t.Fatal("unattested key store must refuse to sign")
	} else if !strings.Contains(err.Error(), "no isolation attestation") {
		t.Fatalf("expected the missing-attestation refusal, got: %v", err)
	}

	// SELF-asserted mechanisms are refused: 0700 excludes other uids, not
	// same-uid agents.
	selfDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(selfDir, IsolationAttestFile),
		[]byte(`{"mechanism":"process-boundary+0700-keystore","key_owner_uid":`+fmt.Sprint(os.Getuid())+`,"agents_excluded":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSigner(selfDir, "herdforge", root); err == nil {
		t.Fatal("self-asserted isolation must refuse to sign")
	}
	if err := WriteIsolationAttestation(selfDir, "self"); err == nil {
		t.Fatal("refusing to RECORD a self-asserted mechanism is part of the contract")
	}

	// An EXTERNAL boundary lets signing proceed.
	ok := attestedKeyDir(t)
	if _, err := LoadOrCreateSigner(ok, "herdforge", root); err != nil {
		t.Fatalf("externally attested key store must sign: %v", err)
	}

	// Tampered attestations fail closed.
	attest := filepath.Join(ok, IsolationAttestFile)
	for name, body := range map[string]string{
		"agents not excluded": `{"mechanism":"test-sandbox","key_owner_uid":` + fmt.Sprint(os.Getuid()) + `,"agents_excluded":false}`,
		"corrupt":             "{not json",
		"foreign uid":         `{"mechanism":"test-sandbox","key_owner_uid":999999,"agents_excluded":true}`,
	} {
		if err := os.WriteFile(attest, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateSigner(ok, "herdforge", root); err == nil {
			t.Fatalf("%s attestation must fail closed", name)
		}
	}
}

// FAC-145: a no-origin repository resolves to ONE identity across all its
// worktrees (the common git dir), while same-named distinct repositories
// stay distinct.
func TestRepositoryIdentity_StableAcrossWorktrees(t *testing.T) {
	root := t.TempDir()
	run := func(dir string, args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "init", "-b", "main")
	run(root, "commit", "--allow-empty", "-m", "seed")
	wt := filepath.Join(root, "wt-a")
	run(root, "worktree", "add", "--detach", wt, "HEAD")

	idRoot, err := RepositoryIdentity(root, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	idWorktree, err := RepositoryIdentity(wt, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if idRoot != idWorktree {
		t.Fatalf("no-origin repo must have ONE identity across worktrees: %s vs %s", idRoot, idWorktree)
	}

	other := t.TempDir()
	run(other, "init", "-b", "main")
	idOther, err := RepositoryIdentity(other, "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if idOther == idRoot {
		t.Fatal("distinct same-named repositories must not collide")
	}
	// 128-bit namespace.
	if parts := strings.Split(idRoot, "-"); len(parts[len(parts)-1]) != 32 {
		t.Fatalf("identity namespace must be 128 bits, got %q", idRoot)
	}
}
