package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/resources"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

type taskSourceFixture struct {
	root, relative, path, receipt, admin string
	entry                                worktreeEntry
}

func taskSourceGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newTaskSourceFixture(t *testing.T) taskSourceFixture {
	t.Helper()
	root, candidate, binding := publicEntryFixture(t)
	t.Setenv("HERD_LAUNCH_RECEIPTS", "")
	// Produce the receipt with the shipped native reconciliation entry and
	// actual canonical cross-family ledger, never a self-sealed fake receipt.
	if err := runHarvestVerifyLanded(pinProofBranch, binding); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, ".herd/herd.yaml")
	if err := os.WriteFile(profile, []byte(taskSourceTestConfig), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CONFIG_PATH", profile)
	keyDir := t.TempDir()
	t.Setenv(dispatch.KeyDirEnv, keyDir)
	t.Setenv("HERD_ROLE", "coordinator")
	attestKeyDir(t, keyDir)
	if _, err := dispatch.LoadSignerForConfig("", root); err != nil {
		t.Fatal(err)
	}
	oldAgents := taskSourceAgents
	taskSourceAgents = func() ([]herdr.AgentEntry, error) { return []herdr.AgentEntry{}, nil }
	t.Cleanup(func() { taskSourceAgents = oldAgents })
	f := taskSourceFixture{root: root, relative: ".worktrees/coordinator-completed-task"}
	f.path = filepath.Join(root, f.relative)
	taskSourceGit(t, root, "worktree", "add", f.path, pinProofBranch)
	f.entry = worktreeEntry{Path: f.path, Branch: pinProofBranch, Head: candidate}
	var err error
	f.admin, err = herdr.HarvestRegistrationDir(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.receipt, err = filepath.Rel(root, hsync.ReceiptPath(root, pinProofRef))
	if err != nil {
		t.Fatal(err)
	}
	legacy := launch.Receipt{Accepted: true, Name: "coordinator", Role: "worker", Branch: pinProofBranch, CWD: f.path}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	launchPath := launch.ReceiptPathFor(root)
	if err := os.MkdirAll(filepath.Dir(launchPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launchPath, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	// Unrelated canonical tracked dirt is intentionally retained. Validation
	// proves object/receipt authority; only the selected carrier must be clean.
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("unrelated user edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

const taskSourceTestConfig = "version: '1'\nproject:\n  name: task-source-fixture\ntask_provider:\n  type: memory\n"

func (f taskSourceFixture) enroll(t *testing.T) taskSourceBinding {
	t.Helper()
	b, err := enrollTaskSource(f.root, f.relative, pinProofRef, f.receipt, true)
	if err != nil {
		t.Fatalf("valid completed task enrollment: %v", err)
	}
	return *b
}

func TestTaskSourceEnrollmentRetiresNamedTask(t *testing.T) {
	f := newTaskSourceFixture(t)
	beforeReceipt, err := os.ReadFile(filepath.Join(f.root, f.receipt))
	if err != nil {
		t.Fatal(err)
	}
	beforeLaunch, err := os.ReadFile(launch.ReceiptPathFor(f.root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enrollTaskSource(f.root, f.relative, pinProofRef, f.receipt, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(f.root, taskSourceJournal), filepath.Join(f.admin, taskSourceMarker)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("dry run wrote authority: %s (%v)", path, err)
		}
	}
	f.enroll(t)
	view := loadTaskSources(f.root)
	eligible := reapPulseEligibleWithSources(f.root, []worktreeEntry{f.entry}, f.root, nil, view)
	if len(eligible) != 1 {
		t.Fatalf("enrolled named task was excluded from pulse: %v", eligible)
	}
	landed, kept := classifyReapEntries(f.root, "origin/main", false, []worktreeEntry{f.entry})
	if len(landed) != 1 || len(kept) != 0 || landed[0].taskSource == nil {
		t.Fatalf("enrolled named task was kept: landed=%v kept=%v", landed, kept)
	}
	if err := retireLandedOneWithInspector(f.root, landed[0], runReapGit, taskSourceOwner{}); err != nil {
		t.Fatalf("enrolled task retirement refused: %v", err)
	}
	if _, err := os.Stat(f.path); !os.IsNotExist(err) {
		t.Fatalf("retired task still exists: %v", err)
	}
	if _, err := exec.Command("git", "-C", f.root, "rev-parse", "--verify", "refs/heads/"+f.entry.Branch).Output(); err == nil {
		t.Fatal("retired branch still exists")
	}
	for path, want := range map[string][]byte{filepath.Join(f.root, f.receipt): beforeReceipt, launch.ReceiptPathFor(f.root): beforeLaunch, filepath.Join(f.root, "a"): []byte("unrelated user edit\n")} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("original evidence or user bytes changed: %s: %v", path, err)
		}
	}
}

func TestTaskSourceEnrollmentProtectsLiveHome(t *testing.T) {
	f := newTaskSourceFixture(t)
	taskSourceAgents = func() ([]herdr.AgentEntry, error) { return []herdr.AgentEntry{{Cwd: f.path}}, nil }
	_, err := enrollTaskSource(f.root, f.relative, pinProofRef, f.receipt, true)
	if err == nil || !strings.Contains(err.Error(), "live resident home") {
		t.Fatalf("live resident home was not refused by independent home guard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, taskSourceJournal)); !os.IsNotExist(err) {
		t.Fatalf("refused enrollment published authority: %v", err)
	}
}

func TestTaskSourceEnrollmentProtectsRuntimeProfile(t *testing.T) {
	f := newTaskSourceFixture(t)
	// Prove valid native enrollment first with the ordinary canonical profile.
	if _, err := enrollTaskSource(f.root, f.relative, pinProofRef, f.receipt, false); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(t.TempDir(), "operator.yaml")
	configText := taskSourceTestConfig + "lanes:\n  - name: dormant\n    agent_kind: codex\n    model: test\n    prompt: test\n    worktree: " + f.relative + "\n"
	if err := os.WriteFile(profile, []byte(configText), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CONFIG_PATH", profile)
	_, err := enrollTaskSource(f.root, f.relative, pinProofRef, f.receipt, true)
	if err == nil || !strings.Contains(err.Error(), "configured or live resident home") {
		t.Fatalf("runtime-profile resident home was not refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, taskSourceJournal)); !os.IsNotExist(err) {
		t.Fatalf("runtime-profile refusal published authority: %v", err)
	}
}

func TestTaskSourceEnrollmentRefusesUnprovenAuthority(t *testing.T) {
	for _, name := range []string{"task", "receipt", "ledger", "launch", "unknown-live", "locked", "dirty", "invoker", "foreign-path"} {
		t.Run(name, func(t *testing.T) {
			f := newTaskSourceFixture(t)
			ref, target := pinProofRef, f.relative
			switch name {
			case "task":
				ref = "FAC-1"
			case "receipt":
				r, err := readTaskSourceReceipt(f.root, f.receipt)
				if err != nil {
					t.Fatal(err)
				}
				r.VerificationDigest = "forged-verification"
				r.Digest = r.ComputeDigest()
				raw, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.root, f.receipt), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "ledger":
				if err := os.Rename(filepath.Join(f.root, ".herd/review-ledger.jsonl"), filepath.Join(f.root, ".herd/review-ledger-held.jsonl")); err != nil {
					t.Fatal(err)
				}
			case "launch":
				if err := os.WriteFile(launch.ReceiptPathFor(f.root), []byte(""), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-live":
				taskSourceAgents = func() ([]herdr.AgentEntry, error) { return nil, errors.New("inventory unavailable") }
			case "locked":
				taskSourceGit(t, f.root, "worktree", "lock", f.path)
			case "dirty":
				if err := os.WriteFile(filepath.Join(f.path, "untracked"), []byte("owned"), 0600); err != nil {
					t.Fatal(err)
				}
			case "invoker":
				t.Chdir(f.path)
			case "foreign-path":
				target = "../foreign"
			}
			if _, err := enrollTaskSource(f.root, target, ref, f.receipt, true); err == nil {
				t.Fatalf("unproven %s authority was accepted", name)
			}
			if _, err := os.Stat(filepath.Join(f.root, taskSourceJournal)); !os.IsNotExist(err) {
				t.Fatalf("refused enrollment published authority: %v", err)
			}
		})
	}
}

type taskSourceOwner struct{ usage resources.ProcessUsage }

func (o taskSourceOwner) InUse(context.Context, string) (resources.ProcessUsage, error) {
	return o.usage, nil
}

func TestTaskSourceActRefusesDrift(t *testing.T) {
	for _, name := range []string{"generation", "signature", "receipt", "branch", "head", "missing-index", "recreated", "active", "unknown-owner", "live-home", "locked", "dirty", "unlanded"} {
		t.Run(name, func(t *testing.T) {
			f := newTaskSourceFixture(t)
			f.enroll(t)
			landed, kept := classifyReapEntries(f.root, "origin/main", false, []worktreeEntry{f.entry})
			if len(landed) != 1 {
				t.Fatalf("valid baseline not landed: %v", kept)
			}
			owner := taskSourceOwner{}
			switch name {
			case "generation":
				if err := os.Remove(filepath.Join(f.admin, taskSourceMarker)); err != nil {
					t.Fatal(err)
				}
			case "signature":
				raw, err := os.ReadFile(filepath.Join(f.root, taskSourceJournal))
				if err != nil {
					t.Fatal(err)
				}
				var b taskSourceBinding
				if err := json.Unmarshal(bytes.TrimSpace(raw), &b); err != nil {
					t.Fatal(err)
				}
				b.Signature = "forged"
				raw, err = json.Marshal(b)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.root, taskSourceJournal), append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				if err := os.WriteFile(filepath.Join(f.root, f.receipt), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "branch":
				taskSourceGit(t, f.path, "branch", "-m", "other-task")
			case "head":
				taskSourceGit(t, f.path, "commit", "--allow-empty", "-m", "changed head")
			case "missing-index":
				if err := os.Remove(filepath.Join(f.root, taskSourceJournal)); err != nil {
					t.Fatal(err)
				}
			case "recreated":
				taskSourceGit(t, f.root, "worktree", "remove", f.path)
				taskSourceGit(t, f.root, "worktree", "add", f.path, f.entry.Branch)
			case "active":
				owner.usage.CWD = true
			case "unknown-owner":
				owner.usage.MetadataUnavailable = true
			case "live-home":
				taskSourceAgents = func() ([]herdr.AgentEntry, error) { return []herdr.AgentEntry{{Cwd: f.path}}, nil }
			case "locked":
				taskSourceGit(t, f.root, "worktree", "lock", f.path)
			case "dirty":
				if err := os.WriteFile(filepath.Join(f.path, "owned"), []byte("evidence"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unlanded":
				taskSourceGit(t, f.root, "update-ref", "refs/remotes/origin/main", f.entry.Head+"~2")
			}
			if err := retireLandedOneWithInspector(f.root, landed[0], runReapGit, owner); err == nil {
				t.Fatalf("act accepted %s drift", name)
			}
			if _, err := os.Stat(f.path); err != nil {
				t.Fatalf("refused act removed carrier: %v", err)
			}
		})
	}
}

func TestTaskSourceHardHomes(t *testing.T) {
	root := t.TempDir()
	var err error
	root, err = canonicalWorktreePath(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"orchestrator", "coordinator", "configured", "task"} {
		if err := os.MkdirAll(filepath.Join(root, ".worktrees", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	v := &taskSourceView{root: root, homes: []string{root, filepath.Join(root, ".worktrees/configured")}}
	if err := v.hardHome(worktreeEntry{Path: filepath.Join(root, ".worktrees/task"), Branch: "task/x"}); err != nil {
		t.Fatalf("ordinary existing task baseline was refused: %v", err)
	}
	for _, tc := range []struct{ path, branch, reason string }{
		{filepath.Join(root, ".worktrees/orchestrator"), "task/x", "reserved resident home"},
		{filepath.Join(root, ".worktrees/coordinator"), "task/x", "reserved resident home"},
		{filepath.Join(root, ".worktrees/configured"), "task/x", "invoking, configured or live resident home"},
		{filepath.Join(root, ".worktrees/task"), "standing/builder", "canonical, detached or standing checkout"},
		{filepath.Join(root, ".worktrees/task"), "main", "canonical, detached or standing checkout"},
		{root, "task/x", "invoking, configured or live resident home"},
	} {
		if err := v.hardHome(worktreeEntry{Path: tc.path, Branch: tc.branch}); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Fatalf("hard resident home missed intended guard %q for %s (%s): %v", tc.reason, tc.path, tc.branch, err)
		}
	}
}
