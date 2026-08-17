package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Kampe/Herdforge/pkg/daemon"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/mail"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/toolchild"
	"github.com/Kampe/Herdforge/pkg/winddown"
)

// bumpClaimGeneration drives the durable claim store's generation for the
// fixture task ABOVE the receipt's, leaving the final lease active.
func bumpClaimGeneration(t *testing.T, dir, ref string, target int64) {
	t.Helper()
	st, err := claim.NewSQLiteLeaseStore(filepath.Join(dir, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key := claim.LeaseKey{Repo: dispatch.RepositoryIdentityOrName(dir, "herdforge-test"), Provider: "kaneo", Project: "proj-x", TaskRef: ref}
	ctx := context.Background()
	// Release any live fixture lease first, then reclaim until the target
	// generation is ACTIVE.
	if leases, err := st.ActiveClaims(ctx, time.Now()); err == nil {
		for _, l := range leases {
			if l.LeaseKey == key {
				if _, _, err := st.Release(ctx, key, l.OwnerID, l.Generation, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for {
		lease, err := st.Acquire(ctx, key, "coordinator-test", "worker", "", time.Now(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if lease.Generation >= target {
			return
		}
		if _, _, err := st.Release(ctx, key, "coordinator-test", lease.Generation, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

// acquireFixtureLease acquires (or reuses) the live coordinator lease for
// ref in dir's claim store and returns the receipt binding.
func acquireFixtureLease(t *testing.T, dir, ref string) (string, int64) {
	t.Helper()
	st, err := claim.NewSQLiteLeaseStore(filepath.Join(dir, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key := claim.LeaseKey{Repo: dispatch.RepositoryIdentityOrName(dir, "herdforge-test"), Provider: "kaneo", Project: "proj-x", TaskRef: ref}
	// Reuse the active lease when one exists (repeat receipt issuance).
	if leases, err := st.ActiveClaims(context.Background(), time.Now()); err == nil {
		for _, l := range leases {
			if l.LeaseKey == key {
				return fmt.Sprintf("claim:%d", l.ID), l.Generation
			}
		}
	}
	lease, err := st.Acquire(context.Background(), key, "coordinator-test", "worker", "", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("claim:%d", lease.ID), lease.Generation
}

func mailPost(dir, sender, subject, body string) (*mail.Envelope, error) {
	return mail.NewMailbox(filepath.Join(dir, ".herd", "mail.jsonl")).SendMessage(sender, "coordinator", subject, body)
}

// writeSignedReceipt issues a coordinator-signed FAC-1 receipt into wt: the
// private key lives in keyDir (OUTSIDE repoDir — the worker-readable tree),
// and the public key is published at repoDir/.herd/receipt.pub, mirroring
// production issuance exactly.
func writeSignedReceipt(t *testing.T, keyDir, repoDir, wt string, mutate func(*dispatch.TaskContext)) {
	t.Helper()
	// Receipts are backed by a REAL live lease in the durable claim store —
	// exact-lease validation requires it (FAC-145).
	leaseID, leaseGen := acquireFixtureLease(t, repoDir, "FAC-1")
	tc := dispatch.TaskContext{
		ProviderType:    "kaneo",
		ProjectID:       "proj-x",
		Repository:      dispatch.RepositoryIdentityOrName(repoDir, "herdforge-test"),
		Role:            dispatch.RoleWorker,
		TaskRef:         "FAC-1",
		TaskID:          "t1",
		Branch:          "herd/fac-1",
		BaseSHA:         "abc",
		LeaseID:         leaseID,
		LeaseGeneration: leaseGen,
		LeaseTaskRef:    "FAC-1",
		SessionID:       "worker-fixture",
		AllowedOps:      dispatch.WorkerOps,
		ExpiresAt:       time.Now().Add(time.Hour),
	}
	if mutate != nil {
		mutate(&tc)
	}
	signer := fixtureSigner(t, keyDir, repoDir)
	signed, err := signer.Issue(tc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0755); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(wt, signed); err != nil {
		t.Fatal(err)
	}
	// Mirror production issuance for the primary (unmutated) receipt: the
	// durable canonical copy always exists. Mutated receipts are auxiliary
	// (reviewer/detached/edge fixtures) and never replace the canonical
	// worker authority.
	if mutate == nil {
		if err := dispatch.StoreCanonicalReceipt(repoDir, signed); err != nil {
			t.Fatal(err)
		}
	}
}

// fixtureSigner loads the signer for a fixture repository using the
// TEST's key dir — never the operator's real ~/.herd/keys — and the same
// stable repository identity production derives.
// attestKeyDir simulates the EXTERNAL isolation boundary production
// requires (FAC-133 sandbox / OS keychain / operator provisioning).
func attestKeyDir(t *testing.T, keyDir string) {
	t.Helper()
	if err := dispatch.WriteIsolationAttestation(keyDir, "test-sandbox"); err != nil {
		t.Fatal(err)
	}
}

func fixtureSigner(t *testing.T, keyDir, repoDir string) *dispatch.Signer {
	t.Helper()
	attestKeyDir(t, keyDir)
	identity, err := dispatch.RepositoryIdentity(repoDir, "herdforge-test")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := dispatch.LoadOrCreateSigner(keyDir, identity, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// reviewTestEnv keeps reviewer-lane tests independent of the role marker on
// the pane running the suite. HERD_ROLE is diagnostic launch metadata in
// production, but signer-boundary test fixtures intentionally exercise the
// coordinator path from their child CLI processes.
func reviewTestEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "HERD_ROLE=") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func TestReviewTestEnv_StripsInheritedAgentRole(t *testing.T) {
	t.Setenv("HERD_ROLE", "agent")
	t.Setenv("HERD_FAC356_TEST_SENTINEL", "preserved")

	env := reviewTestEnv()
	for _, entry := range env {
		if strings.HasPrefix(entry, "HERD_ROLE=") {
			t.Fatalf("review test subprocess environment leaked %q", entry)
		}
	}
	found := false
	for _, entry := range env {
		if entry == "HERD_FAC356_TEST_SENTINEL=preserved" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("review test environment dropped unrelated inherited variables")
	}
}

// herdCmd runs the built binary in dir with the coordinator key dir pinned
// to the test's private location.
// provisionFence creates the shared fence store the FAC-145 claim stack
// requires. It is idempotent, so callers can invoke it unconditionally.
//
// FAC-145 routes provider mutations through a shared fence authority; without
// a provisioned volume every mutation refuses with "run herd fence-provision
// on the shared volume". Tests provision inside their OWN temp tree, never a
// shared host path.
// ensureFenceSeal provisions the claim volume once per test dir and returns the
// minted seal. Best-effort: a caller that does not need the fence is unaffected.
func ensureFenceSeal(binary, dir, keyDir string) string {
	if seal := readMintedSeal(dir); seal != "" {
		return seal
	}
	c := exec.Command(binary, "fence-provision")
	c.Dir = dir
	c.Env = append(reviewTestEnv(),
		dispatch.KeyDirEnv+"="+keyDir,
		herdr.NoLiveEnv+"=1",
		herdr.BinaryEnv+"=",
		"HERD_CLAIM_DIR="+filepath.Join(dir, "claims"),
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`HERD_FENCE_VOLUME_ID="([^"]+)"`).FindStringSubmatch(string(out))
	if len(m) != 2 {
		return ""
	}
	_ = os.WriteFile(filepath.Join(dir, ".fence-seal"), []byte(m[1]), 0o600)
	return m[1]
}

// provisionFence guarantees a sealed claim volume. It is idempotent: herdCmd
// provisions lazily, so a store may already be sealed, and fence-provision
// correctly refuses to overwrite one.
func provisionFence(t *testing.T, binary, dir, keyDir string) {
	t.Helper()
	if ensureFenceSeal(binary, dir, keyDir) == "" {
		t.Fatal("fence-provision produced no volume seal")
	}
}

// readMintedSeal returns the seal recorded by provisionFence, or "" before the
// volume has been provisioned.
func readMintedSeal(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".fence-seal"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func herdCmd(binary, dir, keyDir string, args ...string) *exec.Cmd {
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	// FAC-145 hermeticity: a child CLI can NEVER reach the operator's live
	// herdr fleet. Without an explicit fake, any herdr call fails closed.
	cmd.Env = append(reviewTestEnv(),
		dispatch.KeyDirEnv+"="+keyDir,
		herdr.NoLiveEnv+"=1",
		herdr.BinaryEnv+"=",
		// FAC-145 routes provider mutations through a shared fence authority,
		// which refuses without a claim volume. Point it inside the test's own
		// tree so the CLI is fenced but hermetic — never at a shared host path.
		"HERD_CLAIM_DIR="+filepath.Join(dir, "claims"),
		// The fake Kaneo board enforces fence+op with status (it counts
		// patches and checks state), so declare it as an atomic fence
		// server for the fenced mutation path.
		"HERD_FENCE_ATOMIC_SERVER=1",
	)
	// The volume seal is MINTED by fence-provision, never chosen by the caller:
	// WriteSharedMarker clears any preset HERD_FENCE_VOLUME_ID so a host cannot
	// plant a stolen seal. An invented id fails as "volume_seal mismatch (not
	// this fleet store; independent/forged store refused)" -- the gate working.
	//
	// Provisioning is LAZY rather than a call every test must remember: FAC-145
	// routes provider mutations through the fence authority, so any CLI test
	// that mutates needs a sealed volume. Doing it here means one place knows
	// the requirement instead of every fixture shape in the file.
	if seal := ensureFenceSeal(binary, dir, keyDir); seal != "" {
		cmd.Env = append(cmd.Env, "HERD_FENCE_VOLUME_ID="+seal)
	}
	return cmd
}

// herdCmdWithFake is herdCmd plus an injected protocol-faithful herdr fake
// for tests that exercise real launch paths.
// prependToPath inserts dir at the front of PATH in cmd.Env so the
// subprocess can find stub harness binaries (pi, codex, etc.).
func prependToPath(cmd *exec.Cmd, dir string) {
	for i, e := range cmd.Env {
		if strings.HasPrefix(e, "PATH=") {
			// PathListSeparator (":"), not PathSeparator ("/"). Joining with the
			// FILE separator produced ONE malformed entry, so the stub dir was
			// never searched and the launch failed as: lane "assayer" harness
			// "pi" binary not found in $PATH.
			cmd.Env[i] = "PATH=" + dir + string(os.PathListSeparator) + strings.TrimPrefix(e, "PATH=")
			return
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+dir)
}

func herdCmdWithFake(binary, dir, keyDir, fakeBin, fakeLog string, args ...string) *exec.Cmd {
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
 cmd.Env = append(reviewTestEnv(), dispatch.KeyDirEnv+"="+keyDir)
	// Review commands may chdir into an isolated detached worktree. Keep the
	// provider's shared claim fence anchored to the fixture repository rather
	// than making the review checkout provision a second authority.
	cmd.Env = append(cmd.Env, "HERD_CLAIM_DIR="+filepath.Join(dir, "claims"))
	if seal := readMintedSeal(dir); seal != "" {
		cmd.Env = append(cmd.Env, "HERD_FENCE_VOLUME_ID="+seal)
	}
	cmd.Env = append(cmd.Env, hermeticHerdrEnv(fakeBin, fakeLog)...)
	// Pi session allocation and route state must stay inside the fixture:
	// the operator's ~/.pi is neither present in CI nor ours to touch.
	cmd.Env = append(cmd.Env,
		"PI_CODING_AGENT_SESSION_DIR="+filepath.Join(dir, ".herd", "pi-sessions"),
		"HERDR_ROUTE_STATE_DIR="+filepath.Join(dir, ".herd", "route-state"),
	)
	return cmd
}

// fakeKaneo is a minimal stateful board: one in-progress task, PATCH counted.
type fakeKaneo struct {
	mu            sync.Mutex
	status        string
	labels        []string
	lieOnPatch    bool // ack the PATCH but never change state (readback drift)
	lastComment   string
	commentBodies []string
	patches       int32
	comments      int32
}

// verdictComments counts real verdict deliveries, excluding the
// cross-host ownership claim markers the broker writes to serialize
// coordinators (FAC-145).
func (fk *fakeKaneo) verdictComments() int {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	n := 0
	for _, c := range fk.commentBodies {
		if strings.Contains(c, "REVIEW VERDICT") {
			n++
		}
	}
	return n
}

func (fk *fakeKaneo) taskJSON() string {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	labels := "[]"
	if len(fk.labels) > 0 {
		quoted := make([]string, 0, len(fk.labels))
		for _, l := range fk.labels {
			quoted = append(quoted, fmt.Sprintf("%q", l))
		}
		labels = "[" + strings.Join(quoted, ",") + "]"
	}
	return fmt.Sprintf(`{"id":"t1","ref":"FAC-1","title":"One","status":%q,"priority":"high","projectId":"proj-x","labels":%s}`, fk.status, labels)
}

// setLabels publishes review provenance (author-family/author-model/
// candidate-sha) that reviewer routing requires before any tab is created.
func (fk *fakeKaneo) setLabels(labels ...string) {
	fk.mu.Lock()
	defer fk.mu.Unlock()
	fk.labels = append([]string(nil), labels...)
}

func newFakeKaneo() (*fakeKaneo, *httptest.Server) {
	fk := &fakeKaneo{status: "in-progress"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/task", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "[%s]", fk.taskJSON())
	})
	mux.HandleFunc("/api/task/t1/comment", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fk.mu.Lock()
			list := make([]map[string]string, 0, len(fk.commentBodies))
			for _, c := range fk.commentBodies {
				list = append(list, map[string]string{"content": c})
			}
			fk.mu.Unlock()
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Body    string `json:"body"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal(body, &payload)
		if payload.Content == "" {
			payload.Content = payload.Body
		}
		if payload.Content == "" {
			payload.Content = string(body)
		}
		fk.mu.Lock()
		fk.lastComment = string(body)
		fk.commentBodies = append(fk.commentBodies, payload.Content)
		fk.mu.Unlock()
		atomic.AddInt32(&fk.comments, 1)
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/task/t1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch || r.Method == http.MethodPut {
			var body struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fk.mu.Lock()
			if !fk.lieOnPatch {
				fk.status = body.Status
			}
			fk.mu.Unlock()
			atomic.AddInt32(&fk.patches, 1)
			w.Write([]byte(`{}`))
			return
		}
		fmt.Fprint(w, fk.taskJSON())
	})
	return fk, httptest.NewServer(mux)
}

func writeReviewConfig(t *testing.T, dir, apiURL, projectID string) {
	t.Helper()
	// Canonical-root resolution is mandatory on every authority path; the
	// fixture must be a real repository like production.
	gitIn(t, dir, "init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	// Pi session allocation resolves (and requires) this root; keep it inside
	// the fixture so tests never touch the operator's ~/.pi.
	if err := os.MkdirAll(filepath.Join(dir, ".herd", "pi-sessions"), 0755); err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf(`version: "1"
project:
  name: herdforge-test
  default_branch: main
task_provider:
  type: kaneo
  project_id: %q
  api_url: %q
verification:
  test_command: "echo ok"
lanes:
  - name: assayer
    role: reviewer
    agent_kind: "codex"
    harness: "codex"
    provider: "codex"
    model: "gpt-5.6-luna"
    effort: "medium"
    task_shape: "qa"
    prompt: .herd/prompts/reviewer.md
    worktree: .worktrees/assayer
  - name: smith
    role: worker
    agent_kind: "codex"
    harness: "codex"
    provider: "codex"
    model: "gpt-5.6-luna"
    effort: "medium"
    task_shape: "implementation"
    prompt: .herd/prompts/worker.md
    worktree: .worktrees/smith
`, projectID, apiURL)
	if err := os.WriteFile(filepath.Join(dir, ".herd", "herd.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
}

// seedReviewAdmission seeds the lifecycle to StateBuilding and runs the
// completion gate to persist a passing verification receipt, so the
// subprocess `herd review` can recover the digest and admit the candidate.
// Idempotent: if the lifecycle already records state for ref (e.g.
// approveFixture's seedFixtureLifecycle already ran), the chain append
// is skipped and only the completion gate runs.
func seedReviewAdmission(t *testing.T, dir, ref string) {
	t.Helper()
	wtDir := filepath.Join(dir, ".herd", "worktrees", strings.ToLower(hsync.NormalizeRef(ref)))
	candidateOut, err := exec.Command("git", "-C", wtDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve candidate SHA: %v", err)
	}
	candidate := strings.TrimSpace(string(candidateOut))
	_, leaseGen := acquireFixtureLease(t, dir, ref)

	// Production .gitignore excludes TASK-CONTEXT.json so the verifier's
	// requireCleanCandidate does not flag the launch receipt as dirty.
	// A git worktree has its own root, so the repo-root .gitignore does
	// NOT apply — write one directly into the worktree.
	gitignorePath := filepath.Join(wtDir, ".gitignore")
	if _, err := os.Stat(gitignorePath); err != nil {
		if err := os.WriteFile(gitignorePath, []byte("TASK-CONTEXT.json\n.gitignore\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Seed the lifecycle chain to "building" if not already seeded.
	// approveFixture's seedFixtureLifecycle may have already done this.
	machine, err := lifecycle.NewMachine(filepath.Join(dir, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatalf("lifecycle machine: %v", err)
	}
	st, _ := machine.EventStore().CurrentState(hsync.NormalizeRef(ref))
	machine.Close()
	if st == nil || st.LeaseGeneration == 0 {
		store, err := lifecycle.NewEventStore(filepath.Join(dir, ".herd", "lifecycle.db"))
		if err != nil {
			t.Fatalf("lifecycle store: %v", err)
		}
		defer store.Close()
		tx, err := store.DB().Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		chain := []lifecycle.State{
			lifecycle.StateEligible, lifecycle.StateClaimed, lifecycle.StateDispatched,
			lifecycle.StateBuilding,
		}
		for i, to := range chain {
			if _, err := store.AppendTx(tx, lifecycle.AppendIntent{
				TaskRef: hsync.NormalizeRef(ref), Repo: "herdforge-test", To: to,
				Actor: "fixture", IdempotencyKey: fmt.Sprintf("fixture-review-%s-%d", ref, i),
				LeaseGeneration: leaseGen, ProviderRevision: "provider-rev-1",
				Branch: "herd/fac-1", CandidateSHA: candidate,
			}); err != nil {
				_ = tx.Rollback()
				t.Fatalf("lifecycle append %s: %v", to, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit lifecycle: %v", err)
		}
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(prev) }()

	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	wtRel := worktreePathForRef(ref)
	if _, _, _, err := verifyWorktreeForReview(context.Background(), cfg, ref, wtRel); err != nil {
		t.Fatalf("seed review admission (completion gate): %v", err)
	}
}

// FAC-145: the review status transition goes through the context-bound
// provider — with a full binding it lands exactly one status write.
func TestReviewCLI_BoundStatusTransition(t *testing.T) {
	binary := buildHerd(t)
	fk, server := newFakeKaneo()
	defer server.Close()

	dir, keyDir := t.TempDir(), t.TempDir()
	attestKeyDir(t, keyDir)
	writeReviewConfig(t, dir, server.URL, "proj-x")
	// The completion gate (admitWorktreeForReview) resolves the candidate
	// HEAD from the worktree, so the fixture needs a REAL git worktree —
	// not just a directory with a receipt file.
	gitIn(t, dir, "commit", "--allow-empty", "-q", "-m", "base")
	wtDir := filepath.Join(dir, ".herd", "worktrees", "fac-1")
	gitIn(t, dir, "worktree", "add", "-b", "herd/fac-1", wtDir, "HEAD")
	// The transition is bound to the dispatched task's own receipt in the
	// managed worktree (FAC-145 — no config-derived synthetic context).
	writeSignedReceipt(t, keyDir, dir, wtDir, nil)
	seedReviewAdmission(t, dir, "FAC-1")

	out, err := herdCmd(binary, dir, keyDir, "review", "FAC-1").CombinedOutput()
	if err != nil {
		t.Fatalf("herd review failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "moved card [FAC-1]") {
		t.Fatalf("expected in-review move, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 1 {
		t.Fatalf("expected exactly 1 status write, saw %d", got)
	}
}

// FAC-145: a failed verify in a worktree carrying a launch receipt posts a
// worker FAIL callback bound to the receipt's repo + lease + task identity.
func TestVerifyCLI_PostsReceiptBoundFailCallback(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir := t.TempDir(), t.TempDir()
	attestKeyDir(t, keyDir)
	if err := os.MkdirAll(filepath.Join(dir, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-b", "main")
	wt := filepath.Join(dir, "wt")
	writeSignedReceipt(t, keyDir, dir, wt, func(tc *dispatch.TaskContext) { tc.LeaseGeneration = 5 })

	out, err := herdCmd(binary, dir, keyDir, "verify", "wt").CombinedOutput()
	if err == nil {
		t.Fatalf("verify of an empty worktree must fail:\n%s", out)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".herd", "mail.jsonl"))
	if err != nil {
		t.Fatalf("no callback posted to the coordinator mailbox: %v\n%s", err, out)
	}
	var env struct {
		Recipient string `json:"recipient"`
		Subject   string `json:"subject"`
		Body      string `json:"body"`
	}
	line := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("mailbox line not an envelope: %v\n%s", err, line)
	}
	if env.Recipient != "coordinator" || !strings.HasPrefix(env.Subject, "blocked: FAC-1") {
		t.Fatalf("envelope not a coordinator FAIL callback: %+v", env)
	}
	var cb struct {
		Ref             string `json:"ref"`
		Kind            string `json:"kind"`
		Repo            string `json:"repo"`
		LeaseGeneration int64  `json:"lease_generation"`
		SenderRole      string `json:"sender_role"`
	}
	if err := json.Unmarshal([]byte(env.Body), &cb); err != nil {
		t.Fatalf("callback body: %v", err)
	}
	wantRepo := dispatch.RepositoryIdentityOrName(dir, "herdforge-test")
	if cb.Ref != "FAC-1" || cb.Kind != "blocked" || cb.Repo != wantRepo ||
		cb.LeaseGeneration != 5 || cb.SenderRole != "worker" {
		t.Fatalf("callback not bound to the receipt: %+v", cb)
	}
}

// gitIn lives in testsfor_cli_test.go (main) — it already nulls the host's
// global/system git config, so fixtures stay hermetic. Do not re-declare it.

// approveFixture builds a repo whose origin/main carries FAC-1 merge
// evidence, a fake kaneo board with FAC-1 in-review, and a managed worktree
// receipt holding lease generation 3.
// seedCompletionReceipt gives a fixture card the FAC-132 closing authority
// `herd approve` now requires: a real merged candidate on origin/main, a
// sealed task-bound receipt over it, and durable lifecycle state at
// "integrated" for the same lease generation and candidate.
func seedCompletionReceipt(t *testing.T, dir, ref string, leaseGen int64) {
	t.Helper()
	// A candidate commit with actual content, merged into main so the
	// receipt's merge SHA is an ancestor of origin/main and carries the
	// candidate's patch id (an empty commit would have none).
	base := gitIn(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "landed.txt"), []byte(ref+" landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "landed.txt")
	gitIn(t, dir, "commit", "-m", "feat: "+ref+" candidate")
	merge := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "push", "-q", "origin", "HEAD:main")
	gitIn(t, dir, "fetch", "-q", "origin")

	patch, err := hsync.PatchID(dir, merge)
	if err != nil {
		t.Fatalf("patch id: %v", err)
	}
	repoID, err := toolchild.RepositoryIdentity(dir)
	if err != nil {
		t.Fatalf("repository identity: %v", err)
	}
	r := &hsync.CompletionReceipt{
		RepoID: repoID, TaskRef: ref, TaskID: "t1",
		ProviderRevision: "provider-rev-1", LeaseGeneration: leaseGen,
		BaseSHA: base, CandidateSHA: merge, MergeSHA: merge,
		PatchID: patch, AcceptanceDigest: "acceptance-digest-1",
		VerificationDigest: "verification-digest-1", RiskTier: "R3",
		AuthorFamily: "anthropic", ReviewerFamily: "openai",
		Verdict: "PASS", IntegrationResult: hsync.IntegrationMerged,
	}
	r.Seal()
	if err := hsync.WriteReceipt(dir, r); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
	seedIntegratedLifecycle(t, dir, ref, leaseGen, merge)
}

// seedIntegratedLifecycle walks the durable lifecycle chain to "integrated";
// the state machine only accepts legal transitions, so the whole path runs.
func seedIntegratedLifecycle(t *testing.T, dir, ref string, leaseGen int64, candidate string) {
	t.Helper()
	store, err := lifecycle.NewEventStore(lifecycle.CanonicalStatePath(dir))
	if err != nil {
		t.Fatalf("lifecycle store: %v", err)
	}
	defer store.Close()
	tx, err := store.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	chain := []lifecycle.State{
		lifecycle.StateEligible, lifecycle.StateClaimed, lifecycle.StateDispatched,
		lifecycle.StateBuilding, lifecycle.StateVerifying, lifecycle.StateReviewing,
		lifecycle.StateIntegrationQueued, lifecycle.StateIntegrated,
	}
	for i, to := range chain {
		if _, err := store.AppendTx(tx, lifecycle.AppendIntent{
			TaskRef: hsync.NormalizeRef(ref), Repo: "herdforge-test", To: to,
			Actor: "fixture", IdempotencyKey: fmt.Sprintf("fixture-%s-%d", ref, i),
			LeaseGeneration: leaseGen, ProviderRevision: "provider-rev-1",
			Branch: "herd/fac-1", CandidateSHA: candidate,
		}); err != nil {
			_ = tx.Rollback()
			t.Fatalf("lifecycle append %s: %v", to, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit lifecycle: %v", err)
	}
}

// seedDisabledWinddown writes a durable, explicitly-disabled wind-down state
// into a fixture repo so `herd` subprocesses pass fleet admission.
func seedDisabledWinddown(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, ".herd", "winddown.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := winddown.New(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(context.Background(), false, "test", "fixture-admission", 1, nil); err != nil {
		t.Fatal(err)
	}
}

func approveFixture(t *testing.T) (dir, keyDir string, fk *fakeKaneo) {
	t.Helper()
	fk, server := newFakeKaneo()
	t.Cleanup(server.Close)
	fk.mu.Lock()
	fk.status = "in-review"
	fk.mu.Unlock()

	dir, keyDir = t.TempDir(), t.TempDir()
	attestKeyDir(t, keyDir)
	writeReviewConfig(t, dir, server.URL, "proj-x")

	gitIn(t, dir, "init", "-b", "main")
	gitIn(t, dir, "commit", "--allow-empty", "-m", "feat: FAC-1 landed")
	bare := t.TempDir()
	gitIn(t, dir, "clone", "--bare", ".", filepath.Join(bare, "origin.git"))
	gitIn(t, dir, "remote", "add", "origin", filepath.Join(bare, "origin.git"))
	gitIn(t, dir, "fetch", "-q", "origin")

	// Fleet admission reads .herd/winddown.json relative to the CLI's cwd and
	// rejects when it is missing. Seed an explicitly DISABLED state so these
	// tests exercise their own subject rather than the admission gate.
	seedDisabledWinddown(t, dir)

	writeSignedReceipt(t, keyDir, dir, filepath.Join(dir, ".herd", "worktrees", "fac-1"), nil)

	// FAC-132 gates the board move on a task-bound completion receipt, so the
	// fixture supplies one under the SAME lease generation as the launch
	// receipt above; these tests are about the FAC-145 machinery around the
	// move, not about which authority permits it.
	_, leaseGen := acquireFixtureLease(t, dir, "FAC-1")
	// FAC-147 binds the completion gate to the LIFECYCLE's recorded lease, not
	// the claim store's. Acquiring a claim is not enough: bindingForWorktree
	// reads machine.EventStore().CurrentState(ref).LeaseGeneration and refuses
	// with "no positive lease generation (lifecycle must record the active
	// lease)". Seed the same generation into the lifecycle so the two agree.
	seedFixtureLifecycle(t, dir, "FAC-1", leaseGen)
	seedCompletionReceipt(t, dir, "FAC-1", leaseGen)
	return dir, keyDir, fk
}

// FAC-145: approve mutates through the receipt-bound provider and raises
// the coordinator PASS callback with the receipt's binding.
func TestApproveCLI_BoundMutationAndPassCallback(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)

	out, err := herdCmd(binary, dir, keyDir, "approve", "FAC-1").CombinedOutput()
	if err != nil {
		t.Fatalf("approve failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "APPROVED [FAC-1]") {
		t.Fatalf("expected approval, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 1 {
		t.Fatalf("expected exactly 1 status write, saw %d", got)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".herd", "mail.jsonl"))
	if err != nil {
		t.Fatalf("coordinator PASS callback not posted: %v", err)
	}
	var env struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]), &env); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env.Subject, "complete: FAC-1") {
		t.Fatalf("subject: %q", env.Subject)
	}
	var cb struct {
		Repo            string `json:"repo"`
		LeaseGeneration int64  `json:"lease_generation"`
		SenderRole      string `json:"sender_role"`
		SHA             string `json:"sha"`
	}
	if err := json.Unmarshal([]byte(env.Body), &cb); err != nil {
		t.Fatal(err)
	}
	if cb.Repo != dispatch.RepositoryIdentityOrName(dir, "herdforge-test") || cb.LeaseGeneration != 1 || cb.SenderRole != "coordinator" || cb.SHA == "" {
		t.Fatalf("PASS callback not receipt-bound: %+v", cb)
	}
}

// fixtureEvidenceSHA returns origin/main's head — the FAC-1 merge evidence.
func fixtureEvidenceSHA(t *testing.T, dir string) string {
	t.Helper()
	shaOut, err := exec.Command("git", "-C", dir, "rev-parse", "origin/main").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(shaOut))
}

// writeIntentRecord writes one coordinator-signed, chained journal record,
// exactly as an interrupted approveOne would have (same package: reuses the
// production signing/chaining/anchoring path).
func writeIntentRecord(t *testing.T, dir, keyDir, ref, sha, state string) string {
	t.Helper()
	signer := fixtureSigner(t, keyDir, dir)
	// The journal record carries the EXACT live receipt identity (an
	// interrupted approveOne would have journaled these very fields).
	rc, err := dispatch.LoadCanonicalReceipt(dir, ref)
	if err != nil {
		t.Fatal(err)
	}
	dedupeID := fmt.Sprintf("approve:%s:%s:%s:gen%d", rc.Repository, ref, sha, rc.LeaseGeneration)
	if err := appendApproveIntent(dir, signer, approveIntent{
		Repository:      rc.Repository,
		ProviderType:    rc.ProviderType,
		ProjectID:       rc.ProjectID,
		Ref:             ref,
		TaskID:          rc.TaskID,
		SHA:             sha,
		LeaseID:         rc.LeaseID,
		LeaseGeneration: rc.LeaseGeneration,
		DedupeID:        dedupeID,
		State:           state,
	}); err != nil {
		t.Fatal(err)
	}
	return dedupeID
}

// FAC-145 crash safety: a journaled approval intent with no done record
// (crash between callback and board move) is completed by the next approve
// run, the journal converges to done, and the replayed callback dedupes to
// ONE delivered envelope.
func TestApproveCLI_ReconcilesInterruptedTransition(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	sha := fixtureEvidenceSHA(t, dir)
	dedupeID := writeIntentRecord(t, dir, keyDir, "FAC-1", sha, "intent")

	// Simulate the crashed run's already-delivered callback: the reconcile
	// replay must dedupe against it, not append a duplicate.
	rcGen, err := dispatch.LoadCanonicalReceipt(dir, "FAC-1")
	if err != nil {
		t.Fatal(err)
	}
	crashCb := fmt.Sprintf(`{"ref":"FAC-1","kind":"complete","sha":%q,"repo":%q,"lease_generation":%d,"sender_role":"coordinator","dedupe_id":%q}`,
		sha, dispatch.RepositoryIdentityOrName(dir, "herdforge-test"), rcGen.LeaseGeneration, dedupeID)
	if _, err := mailPost(dir, "coordinator", "complete: FAC-1", crashCb); err != nil {
		t.Fatal(err)
	}

	out, err := herdCmd(binary, dir, keyDir, "approve").CombinedOutput()
	if err != nil {
		t.Fatalf("reconciling approve failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "RECONCILE [FAC-1]") {
		t.Fatalf("expected reconcile pass, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 1 {
		t.Fatalf("reconcile must land exactly 1 status write, saw %d", got)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".herd", "approve-intents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !strings.Contains(lines[len(lines)-1], `"state":"published"`) {
		t.Fatalf("journal must converge to published:\n%s", data)
	}

	// Stable dedupe: the replayed PASS callback converged on the crashed
	// run's envelope — exactly ONE complete callback on the bus.
	mailData, err := os.ReadFile(filepath.Join(dir, ".herd", "mail.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(mailData), `"complete: FAC-1"`); got != 1 {
		t.Fatalf("expected exactly 1 deduped PASS callback envelope, got %d:\n%s", got, mailData)
	}

	// A second run has nothing pending and performs no further writes.
	out, err = herdCmd(binary, dir, keyDir, "approve").CombinedOutput()
	if err != nil {
		t.Fatalf("idempotent re-run failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "RECONCILE") {
		t.Fatalf("done intent must not reconcile again:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 1 {
		t.Fatalf("idempotent re-run must not write, saw %d total", got)
	}
}

// FAC-145 fail-closed recovery: a torn/malformed journal line refuses
// reconciliation outright instead of silently dropping the record, and a
// tampered (signature-broken) record is rejected the same way.
func TestApproveCLI_TornOrTamperedJournalFailsClosed(t *testing.T) {
	binary := buildHerd(t)

	t.Run("torn record", func(t *testing.T) {
		dir, keyDir, fk := approveFixture(t)
		torn := `{"ref":"FAC-1","sha":"abc","dedupe_id":"x","state":"int` // truncated mid-record
		if err := os.WriteFile(filepath.Join(dir, ".herd", "approve-intents.jsonl"), []byte(torn), 0600); err != nil {
			t.Fatal(err)
		}
		out, err := herdCmd(binary, dir, keyDir, "approve").CombinedOutput()
		if err == nil {
			t.Fatalf("torn journal must refuse reconciliation:\n%s", out)
		}
		if !strings.Contains(string(out), "malformed or torn") {
			t.Fatalf("expected torn-journal refusal, got:\n%s", out)
		}
		if got := atomic.LoadInt32(&fk.patches); got != 0 {
			t.Fatalf("torn journal still performed %d board write(s)", got)
		}
	})

	t.Run("tampered record", func(t *testing.T) {
		dir, keyDir, fk := approveFixture(t)
		sha := fixtureEvidenceSHA(t, dir)
		writeIntentRecord(t, dir, keyDir, "FAC-1", sha, "intent")
		journal := filepath.Join(dir, ".herd", "approve-intents.jsonl")
		data, err := os.ReadFile(journal)
		if err != nil {
			t.Fatal(err)
		}
		// A worker retargeting the journaled evidence breaks the signature.
		forged := strings.Replace(string(data), sha, strings.Repeat("f", len(sha)), 1)
		if forged == string(data) {
			t.Fatal("fixture drift: sha not found to tamper")
		}
		if err := os.WriteFile(journal, []byte(forged), 0600); err != nil {
			t.Fatal(err)
		}
		out, err := herdCmd(binary, dir, keyDir, "approve").CombinedOutput()
		if err == nil {
			t.Fatalf("tampered journal must refuse reconciliation:\n%s", out)
		}
		if !strings.Contains(string(out), "signature verification failed") {
			t.Fatalf("expected tampered-journal refusal, got:\n%s", out)
		}
		if got := atomic.LoadInt32(&fk.patches); got != 0 {
			t.Fatalf("tampered journal still performed %d board write(s)", got)
		}
	})
}

// FAC-145 post-board/pre-callback crash window: a "done" record with no
// "published" record is completed by PUBLICATION ONLY — the board is not
// touched again, exactly one deduped PASS callback lands, and the journal
// converges to published.
func TestApproveCLI_DoneWithoutPublishReconcilesByPublicationOnly(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	sha := fixtureEvidenceSHA(t, dir)
	writeIntentRecord(t, dir, keyDir, "FAC-1", sha, "intent")
	writeIntentRecord(t, dir, keyDir, "FAC-1", sha, "done")
	// The board reflects the already-performed mutation.
	fk.mu.Lock()
	fk.status = "done"
	fk.mu.Unlock()

	out, err := herdCmd(binary, dir, keyDir, "approve").CombinedOutput()
	if err != nil {
		t.Fatalf("publication-only reconcile failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "RECONCILE [FAC-1]") || !strings.Contains(string(out), "state done") {
		t.Fatalf("expected done-state reconcile, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 0 {
		t.Fatalf("publication-only reconcile must not touch the board, saw %d write(s)", got)
	}
	mailData, err := os.ReadFile(filepath.Join(dir, ".herd", "mail.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(mailData), `"complete: FAC-1"`); got != 1 {
		t.Fatalf("expected exactly 1 published PASS callback, got %d", got)
	}
	journal, err := os.ReadFile(filepath.Join(dir, ".herd", "approve-intents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	jl := strings.Split(strings.TrimSpace(string(journal)), "\n")
	if !strings.Contains(jl[len(jl)-1], `"state":"published"`) {
		t.Fatalf("journal must converge to published:\n%s", journal)
	}
}

// FAC-145 torn-anchor recovery: a crash between journal append and anchor
// rename (journal exactly one ahead) is healed automatically and the
// operation completes; deeper divergence still fails closed.
func TestApproveCLI_TornAnchorHealsAndCompletes(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	sha := fixtureEvidenceSHA(t, dir)
	writeIntentRecord(t, dir, keyDir, "FAC-1", sha, "intent")
	// Simulate the crash: the anchor for the single record never landed.
	if err := os.Remove(filepath.Join(dir, ".herd", "approve-intents.anchor")); err != nil {
		t.Fatal(err)
	}

	out, err := herdCmd(binary, dir, keyDir, "approve").CombinedOutput()
	if err != nil {
		t.Fatalf("torn-anchor recovery failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "RECONCILE [FAC-1]") {
		t.Fatalf("expected reconcile after heal, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 1 {
		t.Fatalf("healed reconcile must land exactly 1 board write, saw %d", got)
	}
}

// FAC-145 recovery proof: after the ephemeral worktree is reaped, the
// approval still binds through the coordinator's DURABLE canonical receipt
// and completes exactly once.
func TestApproveCLI_RecoversAfterWorktreeLoss(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	// The whole worktree is gone (GC/reap) — only the canonical copy remains.
	if err := os.RemoveAll(filepath.Join(dir, ".herd", "worktrees", "fac-1")); err != nil {
		t.Fatal(err)
	}

	out, err := herdCmd(binary, dir, keyDir, "approve", "FAC-1").CombinedOutput()
	if err != nil {
		t.Fatalf("recovery approve failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "APPROVED [FAC-1]") {
		t.Fatalf("expected approval via canonical receipt, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 1 {
		t.Fatalf("expected exactly 1 board write, saw %d", got)
	}
}

// FAC-145 exact provider readback: a board that ACKS the status write but
// reads back stale state fails the approval — no completion is published,
// the journal never reaches done, and the compensating blocked callback is
// durably posted.
func TestApproveCLI_ReadbackDriftFailsApproval(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	// Lying board: PATCH is acknowledged and counted but state never moves.
	fk.mu.Lock()
	fk.lieOnPatch = true
	fk.mu.Unlock()

	out, err := herdCmd(binary, dir, keyDir, "approve", "FAC-1").CombinedOutput()
	if err == nil {
		t.Fatalf("readback drift must fail the approval:\n%s", out)
	}
	if !strings.Contains(string(out), "reads back as") && !strings.Contains(string(out), "readback") {
		t.Fatalf("expected readback drift refusal, got:\n%s", out)
	}

	mailData, err := os.ReadFile(filepath.Join(dir, ".herd", "mail.jsonl"))
	if err != nil {
		t.Fatalf("compensating callback missing: %v", err)
	}
	if strings.Count(string(mailData), `"complete: FAC-1"`) != 0 {
		t.Fatalf("no completion may be published on readback drift:\n%s", mailData)
	}
	if strings.Count(string(mailData), `"blocked: FAC-1"`) != 1 {
		t.Fatalf("expected exactly 1 compensating blocked callback:\n%s", mailData)
	}

	journal, err := os.ReadFile(filepath.Join(dir, ".herd", "approve-intents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journal), `"state":"done"`) || strings.Contains(string(journal), `"state":"published"`) {
		t.Fatalf("journal must stay at intent on readback drift:\n%s", journal)
	}
}

// FAC-145: a RELEASED newer generation still fences out older receipts —
// the fence is the durable per-key high-water across all lease states,
// never just active claims.
func TestApproveCLI_ReleasedNewerGenerationStillFences(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	// Drive the store to generation 4, then RELEASE it (nothing active).
	bumpClaimGeneration(t, dir, "FAC-1", 4)
	st, err := claim.NewSQLiteLeaseStore(filepath.Join(dir, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	key := claim.LeaseKey{Repo: dispatch.RepositoryIdentityOrName(dir, "herdforge-test"), Provider: "kaneo", Project: "proj-x", TaskRef: "FAC-1"}
	if _, _, err := st.Release(context.Background(), key, "coordinator-test", 4, time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out, err := herdCmd(binary, dir, keyDir, "approve", "FAC-1").CombinedOutput()
	if err == nil {
		t.Fatalf("receipt behind released newer generation must be fenced:\n%s", out)
	}
	if !strings.Contains(string(out), "no ACTIVE lease") {
		t.Fatalf("expected no-active-lease refusal, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 0 {
		t.Fatalf("fenced receipt still performed %d board write(s)", got)
	}
}

// FAC-145 supervised broker lifecycle: ensure spawns a ready broker,
// survives a crash via re-ensure (stale socket replaced), and refuses to
// delete a non-socket at the socket path.
func TestBrokerLifecycle_EnsureCrashRestartAndSocketSafety(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, _ := approveFixture(t)
	sock := shortSocketPath(t)

	ensure := func() ([]byte, error) {
		cmd := herdCmd(binary, dir, keyDir, "broker", "ensure", "--socket", sock)
		return cmd.CombinedOutput()
	}
	out, err := ensure()
	if err != nil {
		t.Fatalf("broker ensure failed: %v\n%s", err, out)
	}
	pidData, err := os.ReadFile(sock + ".pid")
	if err != nil {
		t.Fatalf("pidfile missing: %v", err)
	}
	t.Cleanup(func() {
		if pd, err := os.ReadFile(sock + ".pid"); err == nil {
			var pid int
			fmt.Sscanf(string(pd), "%d", &pid)
			if pid > 1 {
				_ = exec.Command("kill", fmt.Sprint(pid)).Run()
			}
		}
	})

	wt := filepath.Join(dir, ".herd", "worktrees", "fac-1")
	client := func() ([]byte, error) {
		cmd := herdCmd(binary, wt, keyDir, "task", "get", "FAC-1")
		cmd.Env = append(cmd.Env, "HERD_BROKER_SOCK="+sock)
		return cmd.CombinedOutput()
	}
	if out, err := client(); err != nil {
		t.Fatalf("client through ensured broker failed: %v\n%s", err, out)
	}

	// Crash the broker; the socket goes stale.
	var pid int
	fmt.Sscanf(string(pidData), "%d", &pid)
	if pid <= 1 {
		t.Fatalf("bad pid %d", pid)
	}
	if err := exec.Command("kill", fmt.Sprint(pid)).Run(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := client(); err != nil {
			break // broker down observed
		}
		if time.Now().After(deadline) {
			t.Fatal("broker never died")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Re-ensure replaces the stale socket and restores service.
	out, err = ensure()
	if err != nil {
		t.Fatalf("re-ensure after crash failed: %v\n%s", err, out)
	}
	if out, err := client(); err != nil {
		t.Fatalf("client after restart failed: %v\n%s", err, out)
	}

	// Concurrent ensures serialize: no unlinked live socket, no stomped
	// pidfile, service stays up.
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = ensure()
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent ensure %d failed: %v", i, e)
		}
	}
	if out, err := client(); err != nil {
		t.Fatalf("service must survive concurrent ensures: %v\n%s", err, out)
	}

	// Symlink attack: a pre-placed symlink at the pid/log side files must
	// never be followed by coordinator writes.
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	sock2 := shortSocketPath(t)
	_ = os.Remove(sock2 + ".pid")
	if err := os.Symlink(victim, sock2+".pid"); err != nil {
		t.Fatal(err)
	}
	cmdSym := herdCmd(binary, dir, keyDir, "broker", "ensure", "--socket", sock2)
	symOut, symErr := cmdSym.CombinedOutput()
	if symErr == nil {
		t.Fatalf("ensure must fail when the pidfile path is a symlink:\n%s", symOut)
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "original" {
		t.Fatalf("symlinked victim was overwritten: %q %v", string(data), err)
	}

	// Socket-path safety: a plain file at a socket path is never deleted.
	bogus := shortSocketPath(t)
	if err := os.WriteFile(bogus, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := herdCmd(binary, dir, keyDir, "broker", "ensure", "--socket", bogus)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("ensure over a non-socket must refuse:\n%s", out)
	}
	if !strings.Contains(string(out), "not a unix socket") {
		t.Fatalf("expected socket-safety refusal, got:\n%s", out)
	}
	if _, err := os.Stat(bogus); err != nil {
		t.Fatal("non-socket file must not be deleted")
	}
}

// FAC-145 (blocker 1): review runs in a FRESH isolated worktree detached
// at the exact candidate SHA — never the author worktree. The author's
// receipt and branch survive untouched, and the review checkout's identity
// (detached, exact candidate) is verifiable.
func TestReviewCLI_IsolatedDetachedReviewWorktree(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	// herd review claims in-progress cards.
	fk.mu.Lock()
	fk.status = "in-progress"
	fk.mu.Unlock()
	authorDir := filepath.Join(dir, ".herd", "worktrees", "fac-1")

	// Make the author worktree a REAL git worktree on its own branch (the
	// fixture pre-creates the directory for the receipt; git needs it gone).
	if err := os.RemoveAll(authorDir); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "worktree", "add", "-b", "herd/fac-1", authorDir, "HEAD")
	// Auxiliary (mutate != nil) so the canonical store keeps the fixture's
	// original receipt; this test only needs the author worktree copy.
	writeSignedReceipt(t, keyDir, dir, authorDir, func(tc *dispatch.TaskContext) { tc.Branch = "herd/fac-1" })
	seedReviewAdmission(t, dir, "FAC-1")
	authorReceiptBefore, err := os.ReadFile(filepath.Join(authorDir, "TASK-CONTEXT.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate := strings.TrimSpace(runGitOut(t, authorDir, "rev-parse", "HEAD"))
	// Reviewer routing is family-disjoint: it refuses to route without the
	// author's family/model and the exact candidate SHA on the card.
	fk.setLabels("author-family:anthropic", "author-model:claude-sonnet-5", "candidate-sha:"+candidate)

	// herdr is absent in tests, so --spawn exits at the herdr check AFTER
	// the isolated checkout is created; drive the admission directly.
	// The live fleet must be untouched by this test: a protocol-faithful
	// fake herdr is injected, and the operator's workspace census is
	// compared before/after (FAC-145 hermeticity).
	censusBefore := liveWorkspaceCensus(t)
	fakeBin, fakeCalls := installProtocolFakeHerdr(t)
	fakeLog := os.Getenv("HERD_FAKE_LOG")
	// The --spawn path checks exec.LookPath for the lane's harness binary
	// (codex) before creating the isolated checkout, and the write-capable tool
	// probe (pkg/toolprobe) shells the probe command with a sentinel-file prompt. The
	// stub must be probe-faithful (create PROBE_OK.txt and output PROBE_OK) so the probe passes.
	stubDir := stubHarnessPATH(t)
	cmd := herdCmdWithFake(binary, dir, keyDir, fakeBin, fakeLog, "review", "--spawn", "FAC-1")
	prependToPath(cmd, stubDir)
	out, _ := cmd.CombinedOutput()
	reviewDir := filepath.Join(dir, ".herd", "reviews", "fac-1-"+candidate[:12])
	if fi, err := os.Stat(reviewDir); err != nil || !fi.IsDir() {
		t.Fatalf("isolated review worktree missing at %s (out: %s)", reviewDir, out)
	}
	if got := strings.TrimSpace(runGitOut(t, reviewDir, "rev-parse", "HEAD")); got != candidate {
		t.Fatalf("review worktree is at %s, want candidate %s", got, candidate)
	}
	if ref := strings.TrimSpace(runGitOut(t, reviewDir, "symbolic-ref", "-q", "HEAD")); ref != "" {
		t.Fatalf("review worktree must be DETACHED, found %s", ref)
	}
	// The author's worktree and receipt are untouched.
	authorReceiptAfter, err := os.ReadFile(filepath.Join(authorDir, "TASK-CONTEXT.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(authorReceiptAfter) != string(authorReceiptBefore) {
		t.Fatal("review must not replace the author worktree receipt")
	}
	if br := strings.TrimSpace(runGitOut(t, authorDir, "rev-parse", "--abbrev-ref", "HEAD")); br != "herd/fac-1" {
		t.Fatalf("author worktree branch changed to %q", br)
	}

	// The reviewer pane was created through the FAKE with the isolated
	// checkout as its process cwd — never a generic standing tab.
	resolvedReview, rErr := filepath.EvalSymlinks(reviewDir)
	if rErr != nil {
		resolvedReview = reviewDir
	}
	sawTabWithCwd := false
	for _, c := range fakeCalls() {
		if strings.Contains(c, "tab create") &&
			(strings.Contains(c, "--cwd "+reviewDir) || strings.Contains(c, "--cwd "+resolvedReview)) {
			sawTabWithCwd = true
		}
	}
	if !sawTabWithCwd {
		t.Fatalf("reviewer tab must be created with the isolated checkout as cwd; fake saw: %v", fakeCalls())
	}
	// Owner binding requires a REAL agent process in the pane (PID, argv,
	// start token), which a protocol fake cannot provide — so the spawn
	// necessarily fails here. That makes this the compensation proof: a
	// failed admission leaves NO reviewer receipt behind, so no stale
	// authority survives in the isolated checkout. The incarnation-bound
	// session identity itself is covered by pkg/herdr's
	// TestSessionIdentity_BoundToPaneIncarnation.
	if !strings.Contains(string(out), "failed to start reviewer agent") {
		t.Fatalf("expected the hermetic spawn to stop at owner binding; got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(reviewDir, dispatch.TaskContextFile)); !os.IsNotExist(err) {
		t.Fatalf("failed reviewer admission must leave no receipt behind: %v", err)
	}
	assertLiveFleetUntouched(t, censusBefore)
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, _ := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return string(out)
}

// FAC-145 cross-host exactly-once: TWO independent broker processes (each
// with its own socket and its own clone-local lock — the situation a second
// host produces) racing the SAME verdict effect must yield exactly ONE
// verdict comment on the provider. Ownership is decided on the provider
// itself via signed claim markers, so the local lock is not what saves us.
func TestVerdict_TwoBrokersDeliverExactlyOnce(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)

	sockA, sockB := shortSocketPath(t), shortSocketPath(t)
	startBroker(t, binary, dir, keyDir, sockA)
	startBroker(t, binary, dir, keyDir, sockB)

	candidate := fixtureEvidenceSHA(t, dir)
	reviewDir := filepath.Join(dir, ".herd", "reviews", "fac-1-race")
	gitIn(t, dir, "worktree", "add", "--detach", reviewDir, candidate)
	signer := fixtureSigner(t, keyDir, dir)
	leaseID, leaseGen := acquireFixtureLease(t, dir, "FAC-1")
	receipt, err := signer.Issue(dispatch.TaskContext{
		ProviderType: "kaneo", ProjectID: "proj-x",
		Repository: dispatch.RepositoryIdentityOrName(dir, "herdforge-test"),
		Role:       dispatch.RoleReviewer, TaskRef: "FAC-1", TaskID: "t1",
		Branch: "herd/fac-1", BaseSHA: "abc", CandidateSHA: candidate,
		LeaseID: leaseID, LeaseGeneration: leaseGen, LeaseTaskRef: "FAC-1",
		SessionID: "reviewer-race", AllowedOps: dispatch.ReviewerOps,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(reviewDir, receipt); err != nil {
		t.Fatal(err)
	}

	run := func(sock string) error {
		cmd := herdCmd(binary, reviewDir, keyDir, "task", "verdict", "FAC-1", "APPROVED")
		cmd.Env = append(cmd.Env, "HERD_BROKER_SOCK="+sock)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	socks := []string{sockA, sockB}
	for i := range socks {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = run(socks[i]) }(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("broker %d verdict failed: %v", i, e)
		}
	}

	if got := fk.verdictComments(); got != 1 {
		fk.mu.Lock()
		bodies := append([]string(nil), fk.commentBodies...)
		fk.mu.Unlock()
		t.Fatalf("two coordinators produced %d verdict comments, want exactly 1:\n%v", got, bodies)
	}
	// Exactly one consumable authority record too.
	mailData, err := os.ReadFile(filepath.Join(dir, ".herd", "mail.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Count RECORDS (dedupe ids), not the effect id embedded in the
	// canonical body text.
	if n := strings.Count(string(mailData), `\"dedupe_id\":\"verdict-delivered:`); n != 1 {
		t.Fatalf("expected exactly 1 delivered verdict record, got %d:\n%s", n, mailData)
	}
}

// FAC-145 (blocker 2): every isolated-agent class has an explicit
// coordinator admission path — verifier/recovery/integration receipts are
// issued with role-scoped leases and ops, and the verification gate
// refuses a role that may not run it.
func TestReceiptIssueCLI_AdmitsEveryIsolatedAgentClass(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, _ := approveFixture(t)

	for _, role := range []string{"verifier", "recovery", "integration"} {
		target := filepath.Join(dir, ".herd", "worktrees", "fac-1")
		out, err := herdCmd(binary, dir, keyDir, "receipt", "issue", "--role", role, "FAC-1", target).CombinedOutput()
		if err != nil {
			t.Fatalf("%s admission failed: %v\n%s", role, err, out)
		}
		tc, err := dispatch.ReadTaskContext(target)
		if err != nil {
			t.Fatal(err)
		}
		if tc.Role != role {
			t.Fatalf("issued role = %q, want %q", tc.Role, role)
		}
		if tc.LeaseTaskRef != "FAC-1:"+role {
			t.Fatalf("role lease scope = %q", tc.LeaseTaskRef)
		}
		for _, op := range tc.AllowedOps {
			if op == "mutate" {
				t.Fatalf("%s receipt must not carry mutate", role)
			}
		}
	}

	// An unknown class is refused outright.
	if out, err := herdCmd(binary, dir, keyDir, "receipt", "issue", "--role", "merger", "FAC-1", dir).CombinedOutput(); err == nil {
		t.Fatalf("unknown role must be refused:\n%s", out)
	}

	// The verification gate refuses a role that may not run it.
	wt := filepath.Join(dir, ".herd", "worktrees", "fac-1")
	out, err := herdCmd(binary, dir, keyDir, "verify", filepath.Join(".herd", "worktrees", "fac-1")).CombinedOutput()
	if err == nil {
		t.Fatalf("integration receipt must not run the verify gate:\n%s", out)
	}
	if !strings.Contains(string(out), "may not run the verification gate") {
		t.Fatalf("expected gate-role refusal, got:\n%s", out)
	}
	_ = wt
}

// FAC-145 (blocker 1 regression): a REVIEWER receipt whose lease was
// acquired under the scoped key (FAC-X:review) must work through the
// broker. Binding the lease key into the signed receipt is what makes this
// pass; reconstructing an unsuffixed key fails with "no ACTIVE lease".
func TestTaskBrokerCLI_ScopedReviewLeaseWorks(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	sock := shortSocketPath(t)
	startBroker(t, binary, dir, keyDir, sock)

	// Acquire the REVIEW-scoped lease exactly as runReview does.
	st, err := claim.NewSQLiteLeaseStore(filepath.Join(dir, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	reviewKey := claim.LeaseKey{Repo: dispatch.RepositoryIdentityOrName(dir, "herdforge-test"), Provider: "kaneo", Project: "proj-x", TaskRef: "FAC-1:review"}
	lease, err := st.Acquire(context.Background(), reviewKey, "coordinator-review", "reviewer", "", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	signer := fixtureSigner(t, keyDir, dir)
	candidate := fixtureEvidenceSHA(t, dir)
	receipt, err := signer.Issue(dispatch.TaskContext{
		ProviderType: "kaneo", ProjectID: "proj-x", Repository: dispatch.RepositoryIdentityOrName(dir, "herdforge-test"),
		Role: dispatch.RoleReviewer, TaskRef: "FAC-1", TaskID: "t1",
		Branch: "herd/fac-1", BaseSHA: "abc", CandidateSHA: candidate,
		LeaseID: fmt.Sprintf("claim:%d", lease.ID), LeaseGeneration: lease.Generation,
		LeaseTaskRef: "FAC-1:review", SessionID: "reviewer-scoped", AllowedOps: dispatch.ReviewerOps,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A reviewer files verdicts from a REAL checkout pinned to the
	// candidate — the broker re-checks HEAD at verdict time.
	reviewDir := filepath.Join(dir, ".herd", "reviews", "fac-1-scoped")
	gitIn(t, dir, "worktree", "add", "--detach", reviewDir, candidate)
	if err := dispatch.WriteTaskContext(reviewDir, receipt); err != nil {
		t.Fatal(err)
	}

	client := func(args ...string) ([]byte, error) {
		cmd := herdCmd(binary, reviewDir, keyDir, args...)
		cmd.Env = append(cmd.Env, "HERD_BROKER_SOCK="+sock)
		return cmd.CombinedOutput()
	}
	out, err := client("task", "get", "FAC-1")
	if err != nil {
		t.Fatalf("scoped review lease must authorize the broker read: %v\n%s", err, out)
	}
	out, err = client("task", "verdict", "FAC-1", "APPROVED")
	if err != nil {
		t.Fatalf("scoped review lease must authorize the verdict: %v\n%s", err, out)
	}
	if got := fk.verdictComments(); got != 1 {
		t.Fatalf("expected exactly 1 provider verdict comment, saw %d", got)
	}

	// Delivered-marker retry is EXACTLY once: a repeat verdict is inert.
	if out, err = client("task", "verdict", "FAC-1", "APPROVED"); err != nil {
		t.Fatalf("verdict retry must be inert, not an error: %v\n%s", err, out)
	}
	if got := fk.verdictComments(); got != 1 {
		t.Fatalf("verdict retry duplicated the provider comment: %d", got)
	}
}

// FAC-145 (blocker 3): an INTENT-only verdict — provider delivery never
// confirmed — is NEVER consumable as an approval.
func TestVerdict_IntentOnlyNeverConsumable(t *testing.T) {
	dir := t.TempDir()
	mb := mail.NewMailbox(filepath.Join(dir, "mail.jsonl"))
	effect := "herdforge:FAC-1:cafe:gen1:claim:1:APPROVED"
	if _, err := mb.PostCallback("reviewer", mail.Callback{
		Ref: "FAC-1", Kind: mail.CallbackBlocked, SHA: "cafe", Repo: "herdforge",
		LeaseGeneration: 1, SenderRole: "reviewer",
		Detail:   "verdict intent (undelivered)",
		DedupeID: mail.VerdictIntentID(effect),
	}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := mb.EffectiveVerdict("herdforge", "FAC-1", "cafe"); err != nil || found {
		t.Fatalf("intent-only must not resolve as a verdict (found=%v err=%v)", found, err)
	}
	if _, found, err := mb.HasDeliveredVerdict(mail.VerdictEffectID(effect)); err != nil || found {
		t.Fatalf("intent-only must not count as delivered (found=%v err=%v)", found, err)
	}

	// Only the DELIVERED record is consumable.
	if _, err := mb.PostCallback("reviewer", mail.Callback{
		Ref: "FAC-1", Kind: mail.CallbackComplete, SHA: "cafe", Repo: "herdforge",
		LeaseGeneration: 1, SenderRole: "reviewer", Detail: "delivered",
		DedupeID: mail.VerdictEffectID(effect),
	}); err != nil {
		t.Fatal(err)
	}
	eff, found, err := mb.EffectiveVerdict("herdforge", "FAC-1", "cafe")
	if err != nil || !found || eff.Kind != mail.CallbackComplete {
		t.Fatalf("delivered verdict must be consumable: %+v %v %v", eff, found, err)
	}
}

// FAC-145 (blocker 4): the PRODUCTION approval path consumes supersession —
// a delivered REJECTED for the candidate vetoes approval.
func TestApproveCLI_RejectedVerdictVetoesApproval(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	candidate := fixtureEvidenceSHA(t, dir)
	rc, err := dispatch.LoadCanonicalReceipt(dir, "FAC-1")
	if err != nil {
		t.Fatal(err)
	}
	// Stamp the candidate onto the canonical receipt so approval consults
	// the verdict record for this exact commit.
	rc.CandidateSHA = candidate
	signer := fixtureSigner(t, keyDir, dir)
	signed, err := signer.Issue(rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".herd", "worktrees", "fac-1", "TASK-CONTEXT.json"), mustJSON(t, signed), 0644); err != nil {
		t.Fatal(err)
	}

	mb := mail.NewMailbox(filepath.Join(dir, ".herd", "mail.jsonl"))
	if _, err := mb.PostCallback("reviewer", mail.Callback{
		Ref: "FAC-1", Kind: mail.CallbackBlocked, SHA: candidate, Repo: dispatch.RepositoryIdentityOrName(dir, "herdforge-test"),
		LeaseGeneration: rc.LeaseGeneration, SenderRole: "reviewer",
		Detail:   "REVIEW VERDICT FAC-1: REJECTED",
		DedupeID: mail.VerdictEffectID(fmt.Sprintf("herdforge-test:FAC-1:%s:gen%d:%s:REJECTED", candidate, rc.LeaseGeneration, rc.LeaseID)),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := herdCmd(binary, dir, keyDir, "approve", "FAC-1").CombinedOutput()
	if err == nil {
		t.Fatalf("a delivered REJECTED verdict must veto approval:\n%s", out)
	}
	if !strings.Contains(string(out), "effective verdict") {
		t.Fatalf("expected supersession veto, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 0 {
		t.Fatalf("vetoed approval still wrote to the board: %d", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

// FAC-145 dispatch compensation: a dispatch that fails AFTER acquiring its
// claim lease durably releases it — the ticket is never stranded behind a
// dead claim.
func TestDispatchCLI_FailedDispatchReleasesLease(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, _ := approveFixture(t)

	// FAC-9 does not exist on the fake board: dispatch fails after the
	// lease acquisition (no-launch skips broker admission by design).
	out, err := herdCmd(binary, dir, keyDir, "dispatch", "FAC-9", "--no-launch").CombinedOutput()
	if err == nil {
		t.Fatalf("dispatch of a nonexistent ticket must fail:\n%s", out)
	}
	if !strings.Contains(string(out), "lease released") {
		t.Fatalf("expected durable lease compensation, got:\n%s", out)
	}

	st, err := claim.NewSQLiteLeaseStore(filepath.Join(dir, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	leases, err := st.ActiveClaims(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range leases {
		if l.TaskRef == "FAC-9" {
			t.Fatalf("failed dispatch stranded an active lease: %+v", l)
		}
	}
}

// FAC-145: a corrupt or unreadable fence store REFUSES authority — stale
// authority never fails open to generation zero.
func TestApproveCLI_CorruptFenceStoreRefused(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	if err := os.WriteFile(filepath.Join(dir, ".herd", "herdforge.db"), []byte("not a sqlite database"), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := herdCmd(binary, dir, keyDir, "approve", "FAC-1").CombinedOutput()
	if err == nil {
		t.Fatalf("corrupt fence store must refuse approval:\n%s", out)
	}
	if !strings.Contains(string(out), "fence") {
		t.Fatalf("expected fence refusal, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 0 {
		t.Fatalf("corrupt fence still performed %d board write(s)", got)
	}
}

// FAC-145 rollback resistance: deleting a signed done line (truncating at a
// valid boundary), reordering signed lines, or replaying an old signed line
// breaks the chain/anchor and refuses reconciliation — a completed
// transition can never silently become pending again.
func TestApproveCLI_ChainRollbackReorderReplayRefused(t *testing.T) {
	binary := buildHerd(t)

	journalOf := func(dir string) string { return filepath.Join(dir, ".herd", "approve-intents.jsonl") }
	twoRecords := func(t *testing.T) (dir, keyDir string, fk *fakeKaneo, lines []string) {
		dir, keyDir, fk = approveFixture(t)
		sha := fixtureEvidenceSHA(t, dir)
		writeIntentRecord(t, dir, keyDir, "FAC-1", sha, "intent")
		writeIntentRecord(t, dir, keyDir, "FAC-1", sha, "done")
		data, err := os.ReadFile(journalOf(dir))
		if err != nil {
			t.Fatal(err)
		}
		return dir, keyDir, fk, strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	mustRefuse := func(t *testing.T, dir, keyDir, wantMsg string, fk *fakeKaneo) {
		t.Helper()
		out, err := herdCmd(binary, dir, keyDir, "approve").CombinedOutput()
		if err == nil {
			t.Fatalf("must refuse reconciliation:\n%s", out)
		}
		if !strings.Contains(string(out), wantMsg) {
			t.Fatalf("expected %q refusal, got:\n%s", wantMsg, out)
		}
		if got := atomic.LoadInt32(&fk.patches); got != 0 {
			t.Fatalf("refused journal still performed %d board write(s)", got)
		}
	}

	t.Run("rollback: done line deleted at valid boundary", func(t *testing.T) {
		dir, keyDir, fk, lines := twoRecords(t)
		if len(lines) != 2 {
			t.Fatalf("fixture drift: %d lines", len(lines))
		}
		if err := os.WriteFile(journalOf(dir), []byte(lines[0]+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, dir, keyDir, "does not match anchor", fk)
	})

	t.Run("reorder: signed lines swapped", func(t *testing.T) {
		dir, keyDir, fk, lines := twoRecords(t)
		if err := os.WriteFile(journalOf(dir), []byte(lines[1]+"\n"+lines[0]+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, dir, keyDir, "breaks the chain", fk)
	})

	t.Run("replay: old signed line appended again", func(t *testing.T) {
		dir, keyDir, fk, lines := twoRecords(t)
		replay := strings.Join(lines, "\n") + "\n" + lines[0] + "\n"
		if err := os.WriteFile(journalOf(dir), []byte(replay), 0600); err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, dir, keyDir, "breaks the chain", fk)
	})

	t.Run("journal deleted with anchor present", func(t *testing.T) {
		dir, keyDir, fk, _ := twoRecords(t)
		if err := os.Remove(journalOf(dir)); err != nil {
			t.Fatal(err)
		}
		mustRefuse(t, dir, keyDir, "journal deleted or rolled back", fk)
	})
}

// FAC-145 adversarial concurrency: two coordinators approving the same
// card simultaneously serialize on the canonical lock and dedupe on the
// stable callback identity — exactly one board write, one PASS envelope.
func TestApproveCLI_ConcurrentApprovesSerializeAndDedupe(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)

	c1 := herdCmd(binary, dir, keyDir, "approve", "FAC-1")
	c2 := herdCmd(binary, dir, keyDir, "approve", "FAC-1")
	if err := c1.Start(); err != nil {
		t.Fatal(err)
	}
	if err := c2.Start(); err != nil {
		t.Fatal(err)
	}
	err1, err2 := c1.Wait(), c2.Wait()
	// One must approve; the loser either approves nothing (card already
	// done) or reports no in-review match — but never double-writes.
	_ = err1
	_ = err2

	if got := atomic.LoadInt32(&fk.patches); got != 1 {
		t.Fatalf("concurrent approves performed %d board writes, want exactly 1", got)
	}
	mailData, err := os.ReadFile(filepath.Join(dir, ".herd", "mail.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(mailData), `"complete: FAC-1"`); got != 1 {
		t.Fatalf("expected exactly 1 PASS envelope, got %d:\n%s", got, mailData)
	}
}

// Malicious-worker proof at the CLI: a worker that edits its own receipt
// (widening role to coordinator) breaks the coordinator signature and the
// approval refuses with zero provider writes.
func TestApproveCLI_TamperedReceiptRejected(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)

	receiptPath := filepath.Join(dir, ".herd", "worktrees", "fac-1", "TASK-CONTEXT.json")
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Replace(string(data), `"role": "worker"`, `"role": "coordinator"`, 1)
	if forged == string(data) {
		t.Fatal("fixture drift: role field not found to tamper")
	}
	if err := os.WriteFile(receiptPath, []byte(forged), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := herdCmd(binary, dir, keyDir, "approve", "FAC-1").CombinedOutput()
	if err == nil {
		t.Fatalf("tampered receipt must refuse approval:\n%s", out)
	}
	if !strings.Contains(string(out), "signature") {
		t.Fatalf("expected signature rejection, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 0 {
		t.Fatalf("tampered receipt still performed %d status write(s)", got)
	}
}

// FAC-145: a missing receipt on a managed worktree refuses the approval —
// there is NO config-derived fallback.
func TestApproveCLI_MissingReceiptRefusedNoFallback(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)

	// Simulate a receipt that never existed anywhere: neither the worktree
	// copy nor the durable canonical copy remains.
	if err := os.Remove(filepath.Join(dir, ".herd", "worktrees", "fac-1", "TASK-CONTEXT.json")); err != nil {
		t.Fatal(err)
	}
	// Canonical copies are session-keyed; remove the whole store.
	if err := os.RemoveAll(filepath.Join(dir, ".herd", "receipts")); err != nil {
		t.Fatal(err)
	}

	out, err := herdCmd(binary, dir, keyDir, "approve", "FAC-1").CombinedOutput()
	if err == nil {
		t.Fatalf("missing receipt must refuse approval:\n%s", out)
	}
	if !strings.Contains(string(out), "no usable launch receipt") {
		t.Fatalf("expected fail-closed missing-receipt refusal, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 0 {
		t.Fatalf("missing receipt still performed %d status write(s)", got)
	}
}

// shortSocketPath returns a unix socket path short enough for the macOS
// 104-byte sun_path limit, cleaned up with the test.
// socketSeq guarantees per-process uniqueness; nanos alone collide when
// two runs (or two calls) land in the same instant.
var socketSeq atomic.Int64

func shortSocketPath(t *testing.T) string {
	t.Helper()
	// The socket's PARENT must be owned by this uid: the broker refuses a
	// parent owned by anyone else ("refusing socket parent /tmp: owned by uid
	// 0"), which is correct — a world-writable, foreign-owned parent lets
	// another user interpose on the socket. os.TempDir() IS /tmp on Linux CI,
	// so putting the socket straight there failed every broker test while
	// passing on macOS, where TMPDIR is per-user.
	//
	// MkdirTemp creates a 0700 directory owned by us. It stays short because a
	// Unix socket path is capped near 104 bytes, which is this helper's reason
	// for existing.
	parent, err := os.MkdirTemp(os.TempDir(), "hb")
	if err != nil {
		t.Fatalf("socket parent: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	sock := filepath.Join(parent, fmt.Sprintf("%d.sock", socketSeq.Add(1)))
	// Start from a clean slate and remove every side file afterwards: a
	// stale .pid/.log/.lock makes the next run non-deterministic.
	for _, suffix := range []string{"", ".pid", ".log", ".lock"} {
		_ = os.Remove(sock + suffix)
	}
	t.Cleanup(func() {
		for _, suffix := range []string{"", ".pid", ".log", ".lock"} {
			_ = os.Remove(sock + suffix)
		}
	})
	return sock
}

// startBroker launches the coordinator broker for the fixture repo and
// waits for its socket.
func startBroker(t *testing.T, binary, dir, keyDir, sock string) {
	t.Helper()
	startBrokerCmd(t, herdCmd(binary, dir, keyDir, "broker", "--socket", sock), sock)
}

// startBrokerWithFake starts a broker that can reach the injected herdr
// fake, for the paths where the broker itself queries the live fleet.
func startBrokerWithFake(t *testing.T, binary, dir, keyDir, fakeBin, fakeLog, sock string) {
	t.Helper()
	provisionFence(t, binary, dir, keyDir)
	startBrokerCmd(t, herdCmdWithFake(binary, dir, keyDir, fakeBin, fakeLog, "broker", "--socket", sock), sock)
}

func startBrokerCmd(t *testing.T, cmd *exec.Cmd, sock string) {
	t.Helper()
	var brokerOut strings.Builder
	cmd.Stdout = &brokerOut
	cmd.Stderr = &brokerOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("broker socket never appeared; broker output:\n%s", brokerOut.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// FAC-145: the coordinator-owned BROKER is the only provider path for
// agents. The thin client carries just its signed receipt as a capability;
// the broker authenticates and serves ONLY the receipt's own task, exposes
// no mutation op, and refuses tampered receipts — while the agent process
// holds no config, keys, or credentials.
func TestTaskBrokerCLI_ReceiptGated(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	sock := shortSocketPath(t)
	startBroker(t, binary, dir, keyDir, sock)
	wt := filepath.Join(dir, ".herd", "worktrees", "fac-1")

	client := func(args ...string) ([]byte, error) {
		cmd := herdCmd(binary, wt, keyDir, args...)
		cmd.Env = append(cmd.Env, "HERD_BROKER_SOCK="+sock)
		return cmd.CombinedOutput()
	}

	out, err := client("task", "get", "FAC-1", "--full")
	if err != nil {
		t.Fatalf("broker get of own task failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"FAC-1"`) {
		t.Fatalf("broker must return the task:\n%s", out)
	}

	// Worker note comments land with broker-composed attribution.
	out, err = client("task", "comment", "FAC-1", "progress:", "tests", "green")
	if err != nil {
		t.Fatalf("broker comment failed: %v\n%s", err, out)
	}
	if got := atomic.LoadInt32(&fk.comments); got != 1 {
		t.Fatalf("expected 1 provider comment, saw %d", got)
	}
	fk.mu.Lock()
	noteBody := fk.lastComment
	fk.mu.Unlock()
	if !strings.Contains(noteBody, "[note from worker FAC-1]") {
		t.Fatalf("worker note must carry broker-composed attribution:\n%s", noteBody)
	}

	// Verdict forgery: a worker comment phrased as a verdict is REFUSED.
	out, err = client("task", "comment", "FAC-1", "REVIEW", "VERDICT", "FAC-1:", "APPROVED")
	if err == nil {
		t.Fatalf("verdict-phrased worker comment must be refused:\n%s", out)
	}
	if !strings.Contains(string(out), "typed reviewer-only operation") {
		t.Fatalf("expected forgery refusal, got:\n%s", out)
	}

	// Worker receipts cannot invoke the typed verdict op at all.
	out, err = client("task", "verdict", "FAC-1", "APPROVED")
	if err == nil {
		t.Fatalf("worker verdict must be refused:\n%s", out)
	}
	if !strings.Contains(string(out), "reviewer-only") {
		t.Fatalf("expected reviewer-only refusal, got:\n%s", out)
	}

	out, err = client("task", "get", "FAC-2")
	if err == nil {
		t.Fatalf("client must refuse a foreign ref:\n%s", out)
	}
	if !strings.Contains(string(out), "one receipt, one task") {
		t.Fatalf("expected one-receipt-one-task refusal:\n%s", out)
	}

	out, err = client("task", "done", "FAC-1")
	if err == nil {
		t.Fatalf("client must expose no mutation subcommand:\n%s", out)
	}
	if !strings.Contains(string(out), "coordinator-owned") {
		t.Fatalf("expected mutation refusal:\n%s", out)
	}

	// Malicious worker: a role-widened receipt fails BROKER-side
	// authentication — the agent-side check is not the authority.
	receiptPath := filepath.Join(wt, "TASK-CONTEXT.json")
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Replace(string(data), `"role": "worker"`, `"role": "coordinator"`, 1)
	if forged == string(data) {
		t.Fatal("fixture drift: role field not found to tamper")
	}
	if err := os.WriteFile(receiptPath, []byte(forged), 0644); err != nil {
		t.Fatal(err)
	}
	out, err = client("task", "get", "FAC-1")
	if err == nil {
		t.Fatalf("tampered receipt must be refused by the broker:\n%s", out)
	}
	if !strings.Contains(string(out), "signature") && !strings.Contains(string(out), "mutate") {
		t.Fatalf("expected broker-side refusal:\n%s", out)
	}
}

// FAC-145 detached-reviewer proof: a receipt in a directory that is NOT a
// managed worktree — no config, no keys, nothing but the receipt — can read
// its exact card and file its verdict comment through the broker.
func TestTaskBrokerCLI_DetachedReviewerReadAndVerdict(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)
	sock := shortSocketPath(t)
	startBroker(t, binary, dir, keyDir, sock)

	// An isolated review checkout detached at the exact candidate.
	detached := filepath.Join(dir, ".herd", "reviews", "fac-1-detached")
	gitIn(t, dir, "worktree", "add", "--detach", detached, fixtureEvidenceSHA(t, dir))
	writeSignedReceipt(t, keyDir, dir, detached, func(tc *dispatch.TaskContext) {
		tc.Role = dispatch.RoleReviewer
		tc.AllowedOps = dispatch.ReviewerOps
		tc.CandidateSHA = fixtureEvidenceSHA(t, dir)
	})

	client := func(args ...string) ([]byte, error) {
		cmd := herdCmd(binary, detached, keyDir, args...)
		cmd.Env = append(cmd.Env, "HERD_BROKER_SOCK="+sock)
		return cmd.CombinedOutput()
	}
	out, err := client("task", "get", "FAC-1")
	if err != nil {
		t.Fatalf("detached reviewer read failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"FAC-1"`) {
		t.Fatalf("detached reviewer must read its card:\n%s", out)
	}
	out, err = client("task", "verdict", "FAC-1", "APPROVED")
	if err != nil {
		t.Fatalf("detached reviewer verdict failed: %v\n%s", err, out)
	}
	if got := fk.verdictComments(); got != 1 {
		t.Fatalf("expected 1 verdict comment, saw %d", got)
	}
	// Exact readback: the provider received the broker-COMPOSED canonical
	// verdict line bound to the exact candidate.
	fk.mu.Lock()
	verdictBody := fk.lastComment
	fk.mu.Unlock()
	want := "REVIEW VERDICT FAC-1: APPROVED candidate=" + fixtureEvidenceSHA(t, dir)
	if !strings.Contains(verdictBody, want) {
		t.Fatalf("provider must receive the canonical verdict line %q, got:\n%s", want, verdictBody)
	}
	// Durable coordinator-side verdict record on the bus.
	mailData, err := os.ReadFile(filepath.Join(dir, ".herd", "mail.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mailData), "verdict-delivered:"+dispatch.RepositoryIdentityOrName(dir, "herdforge-test")+":FAC-1:") {
		t.Fatalf("durable verdict record missing:\n%s", mailData)
	}
}

// FAC-145: a receipt fenced behind the durable callback high-water// FAC-145: a receipt fenced behind the durable callback high-water
// generation refuses the board mutation entirely.
func TestApproveCLI_StaleLeaseGenerationFencedOut(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, fk := approveFixture(t)
	provisionFence(t, binary, dir, keyDir)

	// The CANONICAL claim store holds a NEWER live lease than the receipt's.
	bumpClaimGeneration(t, dir, "FAC-1", 5)

	out, err := herdCmd(binary, dir, keyDir, "approve", "FAC-1").CombinedOutput()
	if err == nil {
		t.Fatalf("stale-generation approve must fail:\n%s", out)
	}
	if !strings.Contains(string(out), "live lease") {
		t.Fatalf("expected live-lease refusal, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 0 {
		t.Fatalf("fenced-out receipt still performed %d status write(s)", got)
	}
}

// FAC-145: a managed worktree without a valid launch receipt fails verify
// closed — its evidence would be unattributable.
func TestVerifyCLI_ManagedWorktreeWithoutReceiptFailsClosed(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	gitIn(t, dir, "init", "-b", "main")
	wt := filepath.Join(dir, ".herd", "worktrees", "fac-9")
	if err := os.MkdirAll(wt, 0755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "verify", filepath.Join(".herd", "worktrees", "fac-9"))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("must fail closed:\n%s", out)
	}
	if !strings.Contains(string(out), "no valid launch receipt") {
		t.Fatalf("expected receipt fail-closed message, got:\n%s", out)
	}
}

// FAC-145: an empty project binding exits non-zero BEFORE any provider
// mutation — it never becomes an unbound board write.
func TestReviewCLI_MissingProjectFailsBeforeMutation(t *testing.T) {
	binary := buildHerd(t)
	fk, server := newFakeKaneo()
	defer server.Close()

	dir := t.TempDir()
	writeReviewConfig(t, dir, server.URL, "")

	cmd := exec.Command(binary, "review", "FAC-1")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("herd review must fail closed on empty project_id, got:\n%s", out)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 0 {
		t.Fatalf("fail-closed run still performed %d status write(s)", got)
	}
}

// FAC-145: a session-bound receipt's authority dies with its PANE
// INCARNATION. herdr reuses tab/pane ids for whatever occupies the slot
// next, so binding to the slot alone would let a replacement agent revive a
// finished reviewer's authority. The broker must serve the receipt while
// its own incarnation is live and refuse it once the slot changed hands.
//
// Mutation proof: drop terminal_id from the session binding or from the
// liveness comparison and the "replaced" leg below starts succeeding.
func TestBroker_SessionAuthorityDiesWithPaneIncarnation(t *testing.T) {
	binary := buildHerd(t)
	dir, keyDir, _ := approveFixture(t)
	censusBefore := liveWorkspaceCensus(t)
	fakeBin, _ := installProtocolFakeHerdr(t)
	fakeLog := os.Getenv("HERD_FAKE_LOG")

	candidate := fixtureEvidenceSHA(t, dir)
	reviewDir := filepath.Join(dir, ".herd", "reviews", "fac-1-session")
	gitIn(t, dir, "worktree", "add", "--detach", reviewDir, candidate)

	// The reviewer pane herd launched, as herdr reports it.
	setFakePane(t, fakeLog, fakeTerminalID, reviewDir)

	sock := shortSocketPath(t)
	startBrokerWithFake(t, binary, dir, keyDir, fakeBin, fakeLog, sock)

	signer := fixtureSigner(t, keyDir, dir)
	leaseID, leaseGen := acquireFixtureLease(t, dir, "FAC-1")
	receipt, err := signer.Issue(dispatch.TaskContext{
		ProviderType: "kaneo", ProjectID: "proj-x",
		Repository: dispatch.RepositoryIdentityOrName(dir, "herdforge-test"),
		Role:       dispatch.RoleReviewer, TaskRef: "FAC-1", TaskID: "t1",
		Branch: "herd/fac-1", BaseSHA: "abc", CandidateSHA: candidate,
		LeaseID: leaseID, LeaseGeneration: leaseGen, LeaseTaskRef: "FAC-1",
		SessionID: "reviewer-session", AllowedOps: dispatch.ReviewerOps,
		AgentSessionID: fmt.Sprintf("%s/%s/%s", fakeTabID, fakePaneID, fakeTerminalID),
		ExpiresAt:      time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatch.WriteTaskContext(reviewDir, receipt); err != nil {
		t.Fatal(err)
	}

	read := func() ([]byte, error) {
		cmd := herdCmd(binary, reviewDir, keyDir, "task", "get", "FAC-1")
		cmd.Env = append(cmd.Env, "HERD_BROKER_SOCK="+sock)
		return cmd.CombinedOutput()
	}

	out, err := read()
	if err != nil {
		t.Fatalf("receipt must be served while its own pane incarnation is live: %v\n%s", err, out)
	}

	// A replacement agent takes over the same tab/pane slot.
	setFakePane(t, fakeLog, fakeReplacedTerminal, reviewDir)

	out, err = read()
	if err == nil {
		t.Fatalf("a replacement agent in the same tab/pane must NOT inherit the reviewer's authority:\n%s", out)
	}
	if !strings.Contains(string(out), "no longer live") {
		t.Fatalf("expected the session-liveness refusal, got:\n%s", out)
	}
	assertLiveFleetUntouched(t, censusBefore)
}

// seedFixtureLifecycle records an active lease in the durable lifecycle store
// the CLI reads (.herd/lifecycle.db), advancing the task to Building under the
// SAME generation the claim store holds.
func seedFixtureLifecycle(t *testing.T, dir, ref string, leaseGen int64) {
	t.Helper()
	machine, err := lifecycle.NewMachine(filepath.Join(dir, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatalf("lifecycle machine: %v", err)
	}
	defer machine.Close()
	if err := daemon.SeedLifecycleToBuilding(machine, ref, "herdforge", leaseGen); err != nil {
		t.Fatalf("seed lifecycle for %s: %v", ref, err)
	}
}

// TestReviewCLI_CandidateIndexMergedDiscovery verifies that `herd review`
// lists both Kaneo tasks and unintegrated candidates with deterministic ordering
// and structured BLOCKED reasons.
func TestReviewCLI_CandidateIndexMergedDiscovery(t *testing.T) {
	binary := buildHerd(t)
	fk, server := newFakeKaneo()
	defer server.Close()

	dir, keyDir := t.TempDir(), t.TempDir()
	attestKeyDir(t, keyDir)
	writeReviewConfig(t, dir, server.URL, "proj-x")

	// 1. Post a blocked mail callback for FAC-2
	mb := mail.NewMailbox(filepath.Join(dir, ".herd", "mail.jsonl"))
	cb := mail.Callback{
		Ref:    "FAC-2",
		Kind:   mail.CallbackBlocked,
		SHA:    "2222333344445555666677778888999900001111",
		Detail: "missing verification evidence",
	}
	body, _ := json.Marshal(cb)
	_, _ = mb.SendMessage("worker", mail.CoordinatorInbox, "blocked: FAC-2", string(body))

	cmd := herdCmd(binary, dir, keyDir, "review")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd review failed: %v\n%s", err, out)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "[FAC-1]") {
		t.Fatalf("expected FAC-1 in review listing, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "[FAC-2]") {
		t.Fatalf("expected FAC-2 in review listing, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "[BLOCKED: callback blocked: missing verification evidence]") {
		t.Fatalf("expected structured blocked evidence for FAC-2, got:\n%s", outStr)
	}
	if got := atomic.LoadInt32(&fk.patches); got != 0 {
		t.Fatalf("read-only listing must not mutate provider, saw %d patches", got)
	}
}
