package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

// Fixture tuple: production no longer pins a vendor for builder roles, so
// these tests carry their own concrete provider instead of importing one.
const (
	testWorkerProvider = "codex"
	testWorkerModel    = "gpt-5.6-luna"
	testWorkerEffort   = "high"
)

func testLaunchRouter(t *testing.T) *router.SurfaceRouter {
	t.Helper()
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", t.TempDir())
	r := router.NewRouter(nil, nil)
	r.Probes = &router.Probes{
		CLIPresent: func(cli string) bool { return cli == router.PiHarness },
		Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	return r
}

func TestIsAvailable(t *testing.T) {
	// On this machine, herdr should be installed
	available := IsAvailable()
	if !available {
		t.Log("herdr not found in PATH — available=false is expected on CI")
	}
}

func TestAgentListDecodesExactSessionValue(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) != 2 || args[0] != "agent" || args[1] != "list" {
			t.Fatalf("unexpected args: %v", args)
		}
		return `{"result":{"agents":[{"name":"forge-worker","agent":"gpt-5.6-luna","agent_status":"working","pane_id":"pane-1","tab_id":"tab-1","workspace_id":"wF","agent_session":{"value":"session-actual"}}]}}`, nil
	}
	agents, err := AgentList()
	if err != nil || len(agents) != 1 {
		t.Fatalf("AgentList: %#v %v", agents, err)
	}
	if agents[0].Session.Value != "session-actual" {
		t.Fatalf("session=%q", agents[0].Session.Value)
	}
}

func TestProductionRecoveryRebindsDurableProvisionalByExactPaneProcess(t *testing.T) {
	sessionPath := "/sessions/recover.jsonl"
	owner := toolchild.Identity{TabID: "tab-recover", PaneID: "pane-recover", Name: "worker", Provider: router.PiHarness, Repository: "repo-recover", TaskRef: "FAC-188", Role: "worker", Lane: "lane", SessionGeneration: 9, LaunchID: "launch", ArgvDigest: "digest", Argv: []string{"pi", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium", "--session", sessionPath}}
	lc := toolchild.NewLifecycle(owner, &toolchild.FakeTree{}, &toolchild.MemorySink{})
	argvReads := 0
	defer SetPIDArgvReader(func(pid int) ([]string, error) {
		if pid != 501 {
			t.Fatalf("argv reader pid = %d, want 501", pid)
		}
		argvReads++
		return append([]string(nil), owner.Argv...), nil
	})()
	defer SetPiSessionRouteAttesterForTest(func(path string, argv []string) error {
		if path != sessionPath || !equalArgs(argv, owner.Argv) {
			t.Fatalf("attester path=%q argv=%q", path, argv)
		}
		return nil
	})()
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return fmt.Sprintf(`{"result":{"agents":[{"name":"worker","agent":"pi","pane_id":"pane-recover","tab_id":"tab-recover","agent_session":{"value":%q}}]}}`, sessionPath), nil
		}
		return `{"result":{"process_info":{"foreground_processes":[{"pid":501,"name":"node","cwd":"/repo","argv":[]}]}}}`, nil
	}
	oldToken, oldParent := readPIDStartToken, readPIDParent
	defer func() { readPIDStartToken, readPIDParent = oldToken, oldParent }()
	readPIDStartToken = func(pid int) (string, error) { return fmt.Sprintf("start-%d", pid), nil }
	readPIDParent = func(pid int) (int, error) {
		if pid == 501 {
			return 500, nil
		}
		return 1, nil
	}
	if err := bindRecoveredToolChildLifecycle(lc); err != nil {
		t.Fatal(err)
	}
	if !lc.Bound() || lc.Inventory.Owner.PID != 501 || lc.Inventory.Owner.StartToken != "start-501" || lc.Inventory.Owner.SessionID != sessionPath {
		t.Fatalf("recovered owner was not exact: %+v", lc.Inventory.Owner)
	}
	if argvReads == 0 {
		t.Fatal("expected empty pane argv to hydrate via PID argv reader")
	}
}

func TestRecoveryRejectsNonPiProvisionalBeforeInventory(t *testing.T) {
	owner := toolchild.Identity{TabID: "tab-native", PaneID: "pane-native", Name: "worker", Provider: "codex", Repository: "repo", TaskRef: "FAC-NATIVE", Role: "worker", Lane: "worker", SessionGeneration: 1, LaunchID: "launch", ArgvDigest: "digest", Argv: []string{"codex", "--model", "gpt-5.6-luna"}}
	lc := toolchild.NewLifecycle(owner, &toolchild.FakeTree{}, &toolchild.MemorySink{})
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	inventoryCalls := 0
	runHerdr = func(args ...string) (string, error) { inventoryCalls++; return `{}`, nil }
	err := bindRecoveredToolChildLifecycle(lc)
	if err == nil || !strings.Contains(err.Error(), "provisional Pi identity") {
		t.Fatalf("native recovery error = %v", err)
	}
	if inventoryCalls != 0 {
		t.Fatalf("native recovery reached Herdr inventory %d times", inventoryCalls)
	}
}

func TestPaneProcessInfoAcceptsOnlyTypedPaneNotFound(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 4 {
			return `{"error":{"code":"pane_not_found","message":"pane no longer exists"}}`, errors.New("exit status 1")
		}
		return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
	}
	if _, err := paneProcesses("old-pane"); !errors.Is(err, ErrPaneNotFound) {
		t.Fatalf("typed absence = %v", err)
	}
	runHerdr = func(args ...string) (string, error) {
		return `{"error":{"code":"transport_failed"}}`, errors.New("exit status 1")
	}
	if _, err := paneProcesses("old-pane"); errors.Is(err, ErrPaneNotFound) || err == nil {
		t.Fatalf("arbitrary failure accepted: %v", err)
	}
}

func TestVerifyHerdrTerminalRequiresExactTabAndAgentAbsence(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "tab" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) == 2 && args[0] == "agent" {
			return `{"result":{"agents":[]}}`, nil
		}
		return `{"error":{"code":"pane_not_found"}}`, errors.New("exit status 1")
	}
	if err := verifyHerdrTerminal("tab-old", "pane-old"); err != nil {
		t.Fatal(err)
	}
}

func TestHerdrCLINotFound(t *testing.T) {
	// Verify that the error path works when herdr is missing
	// by temporarily modifying PATH
	t.Setenv("PATH", "/dev/null")

	available := IsAvailable()
	if available {
		t.Skip("herdr still found in PATH despite override")
	}
}

func TestEnsureHerdforgeLabel_Prefixes(t *testing.T) {
	got := EnsureHerdforgeLabel("worker")
	want := "Herdforge · worker"
	if got != want {
		t.Errorf("EnsureHerdforgeLabel(\"worker\") = %q, want %q", got, want)
	}
}

func TestEnsureHerdforgeLabel_AlreadyPrefixed(t *testing.T) {
	got := EnsureHerdforgeLabel("Herdforge · worker")
	if got != "Herdforge · worker" {
		t.Errorf("already-prefixed label was modified: %q", got)
	}
}

func TestEnsureHerdforgeLabel_PrefixWithSuffix(t *testing.T) {
	// Already starts with the prefix; extra suffix must not re-prefix.
	got := EnsureHerdforgeLabel("Herdforge · worker (FAC-141)")
	if got != "Herdforge · worker (FAC-141)" {
		t.Errorf("label starting with prefix was modified: %q", got)
	}
}

func TestEnsureHerdforgeLabel_MidStringStillPrefixed(t *testing.T) {
	// Non-vacuous HasPrefix contract: mid-string "Herdforge · " must NOT
	// count as already-prefixed. Mutation of HasPrefix→Contains fails this.
	in := "review of Herdforge · thing"
	got := EnsureHerdforgeLabel(in)
	want := "Herdforge · review of Herdforge · thing"
	if got != want {
		t.Errorf("EnsureHerdforgeLabel(%q) = %q, want %q", in, got, want)
	}
}

// FAC-199 round-2 finding 2: the fleet already carries live labels using the
// older "Herdforge | " prefix (see the ticket's own regression evidence,
// e.g. "Herdforge | FAC-183 | Worker Delivery R1"). Acceptance explicitly
// allows either prefix; a pipe-form label must be recognized as already
// canonical, never rewritten into the dot form.
func TestEnsureHerdforgeLabel_PipeFormAlreadyPrefixed(t *testing.T) {
	got := EnsureHerdforgeLabel("Herdforge | FAC-183 | Worker Delivery R1")
	if got != "Herdforge | FAC-183 | Worker Delivery R1" {
		t.Errorf("pipe-form label was rewritten: %q", got)
	}
}

func TestEnsureHerdforgeLabel_MidStringPipeFormStillPrefixed(t *testing.T) {
	in := "review of Herdforge | thing"
	got := EnsureHerdforgeLabel(in)
	want := "Herdforge · review of Herdforge | thing"
	if got != want {
		t.Errorf("EnsureHerdforgeLabel(%q) = %q, want %q", in, got, want)
	}
}

func TestTabRename_IssuesExactArgv(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	var got []string
	runHerdr = func(args ...string) (string, error) {
		got = append([]string(nil), args...)
		return `{}`, nil
	}
	if err := TabRename("tab-9", "Herdforge · worker"); err != nil {
		t.Fatal(err)
	}
	want := []string{"tab", "rename", "tab-9", "Herdforge · worker"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got, want)
		}
	}
}

func TestTabRename_RequiresTabID(t *testing.T) {
	if err := TabRename("", "label"); err == nil {
		t.Fatal("empty tab id must fail closed")
	}
}

// FAC-199 finding 4: TabRename is the only write path for labels; it must
// enforce the Herdforge prefix itself so no caller can use it to put a raw
// label back onto a live tab, independent of ReconcileHerdforgeLabel.
func TestTabRename_PrefixesRawLabelAsDefenseInDepth(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	var got []string
	runHerdr = func(args ...string) (string, error) {
		got = append([]string(nil), args...)
		return `{}`, nil
	}
	if err := TabRename("tab-9", "task-fac-9-raw"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[3] != "Herdforge · task-fac-9-raw" {
		t.Fatalf("TabRename did not enforce the Herdforge prefix: argv = %v", got)
	}
}

// FAC-199: reconciliation must relabel a live tab in place — never close or
// recreate it — across every raw-label shape the coordinator/worker/reviewer
// fleet can produce, including the mid-string mutation-killer from
// TestEnsureHerdforgeLabel_MidStringStillPrefixed. Every case also proves the
// post-rename readback (finding 1): the stub's "tab list" always reflects
// whatever the most recent "tab rename" actually wrote, so a reconciliation
// that trusted only the rename's exit code (and skipped the readback) would
// still pass here — the dedicated readback-mismatch test below is what kills
// that mutation.
func TestReconcileHerdforgeLabel_Table(t *testing.T) {
	cases := []struct {
		name    string
		current string
		want    string
		renamed bool
	}{
		{name: "already_prefixed", current: "Herdforge · worker", want: "Herdforge · worker", renamed: false},
		{name: "already_prefixed_pipe_form", current: "Herdforge | FAC-183 | Worker Delivery R1", want: "Herdforge | FAC-183 | Worker Delivery R1", renamed: false},
		{name: "raw_task_label", current: "task-fac-183-production-delivery-r1", want: "Herdforge · task-fac-183-production-delivery-r1", renamed: true},
		{name: "malicious_mid_string", current: "review of Herdforge · thing", want: "Herdforge · review of Herdforge · thing", renamed: true},
		{name: "empty_label", current: "", want: "Herdforge · ", renamed: true},
		{name: "recovery_name", current: "recovery-worker-r3", want: "Herdforge · recovery-worker-r3", renamed: true},
		{name: "reviewer_name", current: "reviewer-1", want: "Herdforge · reviewer-1", renamed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := runHerdr
			defer func() { runHerdr = old }()
			liveLabel := tc.current
			var renameArgs []string
			var listCalls int
			runHerdr = func(args ...string) (string, error) {
				if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
					listCalls++
					return fmt.Sprintf(`{"result":{"tabs":[{"tab_id":"tab-x","label":%q,"workspace_id":"wK"}]}}`, liveLabel), nil
				}
				if len(args) == 4 && args[0] == "tab" && args[1] == "rename" {
					renameArgs = append([]string(nil), args...)
					liveLabel = args[3]
					return `{}`, nil
				}
				t.Fatalf("unexpected herdr call: %v", args)
				return "", nil
			}
			label, renamed, err := ReconcileHerdforgeLabel("tab-x", "wK")
			if err != nil {
				t.Fatal(err)
			}
			if label != tc.want {
				t.Fatalf("label = %q, want %q", label, tc.want)
			}
			if renamed != tc.renamed {
				t.Fatalf("renamed = %v, want %v", renamed, tc.renamed)
			}
			switch {
			case tc.renamed && (len(renameArgs) != 4 || renameArgs[2] != "tab-x" || renameArgs[3] != tc.want):
				t.Fatalf("rename argv = %v", renameArgs)
			case tc.renamed && listCalls != 2:
				t.Fatalf("rename must be followed by a readback: tab list calls = %d", listCalls)
			case !tc.renamed && renameArgs != nil:
				t.Fatalf("unexpected rename call for already-prefixed label: %v", renameArgs)
			}
		})
	}
}

func TestReconcileHerdforgeLabel_RequiresWorkspace(t *testing.T) {
	if _, _, err := ReconcileHerdforgeLabel("tab-x", ""); err == nil {
		t.Fatal("empty workspace must fail closed (no unscoped relabel)")
	}
}

// FAC-199 finding 2: a tab id whose live workspace does not match the
// caller's expected workspace must never be relabeled — this is the gate
// that stops a stale/incorrect tab id, or a tab that belongs to some other
// non-Herdforge workspace, from being silently rewritten.
func TestReconcileHerdforgeLabel_RefusesCrossWorkspaceRename(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	var renamed bool
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[{"tab_id":"tab-x","label":"task-fac-9","workspace_id":"w1"}]}}`, nil
		}
		if len(args) == 4 && args[0] == "tab" && args[1] == "rename" {
			renamed = true
			return `{}`, nil
		}
		t.Fatalf("unexpected herdr call: %v", args)
		return "", nil
	}
	if _, _, err := ReconcileHerdforgeLabel("tab-x", "wK"); err == nil {
		t.Fatal("tab in a different workspace must be refused, not relabeled")
	}
	if renamed {
		t.Fatal("cross-workspace refusal must never issue a rename")
	}
}

// FAC-199 finding 1: a `tab rename` that exits 0 but does not actually take
// (herdr bug, truncation, race with another writer) must not be reported as
// success — the post-rename readback is the proof, not the exit code.
func TestReconcileHerdforgeLabel_FailsClosedWhenReadbackDisagreesWithRename(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			// Live label never actually changes, no matter what was renamed.
			return `{"result":{"tabs":[{"tab_id":"tab-x","label":"task-fac-9","workspace_id":"wK"}]}}`, nil
		}
		if len(args) == 4 && args[0] == "tab" && args[1] == "rename" {
			return `{}`, nil // exits clean, but the readback below proves it didn't take
		}
		t.Fatalf("unexpected herdr call: %v", args)
		return "", nil
	}
	if _, renamed, err := ReconcileHerdforgeLabel("tab-x", "wK"); err == nil {
		t.Fatalf("a rename that does not survive readback must be an error, got renamed=%v", renamed)
	}
}

// FAC-199 finding 5: the bounded fleet sweep must repair every named,
// in-workspace raw label, skip tabs already correct, skip unnamed/operator
// tabs (empty label), and never cross a workspace boundary.
// FAC-199 round-2 finding 1: ownership for the sweep must be determined by
// AgentEntry.Name (same doctrine as SelectCleanupCandidates — an unnamed
// pane is the operator's own terminal), never by "does the tab have a
// non-empty label", since a raw operator terminal commonly has a non-empty
// label too ("1", "2", "Terminal", ...). tab-1/tab-2 below model exactly
// that: real tab-list rows with plain numeric/word labels and NO
// corresponding agent entry. The sweep must never even look them up, let
// alone rename them.
func TestReconcileWorkspaceLabels_SweepsOnlyNamedAgentTabsInWorkspace(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	agents := []AgentEntry{
		{Name: "forge-worker", TabID: "tab-raw", PaneID: "pane-raw", Workspace: "wK", Session: struct {
			Value string `json:"value,omitempty"`
		}{Value: "sess-raw"}},
		{Name: "forge-reviewer", TabID: "tab-ok", PaneID: "pane-ok", Workspace: "wK", Session: struct {
			Value string `json:"value,omitempty"`
		}{Value: "sess-ok"}},
		{Name: "forge-other", TabID: "tab-other-w", PaneID: "pane-other", Workspace: "w1", Session: struct {
			Value string `json:"value,omitempty"`
		}{Value: "sess-other"}},
	}
	labels := map[string]string{
		"tab-raw":     "task-fac-183-production-delivery-r1",
		"tab-ok":      "Herdforge · forge-reviewer",
		"tab-other-w": "task-fac-9-other-workspace",
		"tab-1":       "1",
		"tab-2":       "Terminal",
	}
	workspaces := map[string]string{
		"tab-raw": "wK", "tab-ok": "wK", "tab-other-w": "w1", "tab-1": "wK", "tab-2": "wK",
	}
	type row struct {
		TabID     string `json:"tab_id"`
		Label     string `json:"label"`
		Workspace string `json:"workspace_id"`
	}
	var renames []string
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			b, err := json.Marshal(struct {
				Result struct {
					Agents []AgentEntry `json:"agents"`
				} `json:"result"`
			}{Result: struct {
				Agents []AgentEntry `json:"agents"`
			}{Agents: agents}})
			if err != nil {
				t.Fatal(err)
			}
			return string(b), nil
		}
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			var rows []row
			for _, id := range []string{"tab-raw", "tab-ok", "tab-other-w", "tab-1", "tab-2"} {
				rows = append(rows, row{TabID: id, Label: labels[id], Workspace: workspaces[id]})
			}
			b, err := json.Marshal(struct {
				Result struct {
					Tabs []row `json:"tabs"`
				} `json:"result"`
			}{Result: struct {
				Tabs []row `json:"tabs"`
			}{Tabs: rows}})
			if err != nil {
				t.Fatal(err)
			}
			return string(b), nil
		}
		if len(args) == 4 && args[0] == "tab" && args[1] == "rename" {
			renames = append(renames, args[2])
			labels[args[2]] = args[3]
			return `{}`, nil
		}
		t.Fatalf("unexpected herdr call: %v", args)
		return "", nil
	}
	renamed, err := ReconcileWorkspaceLabels("wK")
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed) != 1 || renamed[0] != "tab-raw" {
		t.Fatalf("sweep renamed = %v, want only tab-raw", renamed)
	}
	if len(renames) != 1 || renames[0] != "tab-raw" {
		t.Fatalf("rename calls = %v, want exactly one for tab-raw", renames)
	}
	if labels["tab-1"] != "1" || labels["tab-2"] != "Terminal" {
		t.Fatal("sweep must never touch a tab with no corresponding named agent, even with a non-empty label")
	}
	if labels["tab-other-w"] != "task-fac-9-other-workspace" {
		t.Fatal("sweep must never cross the workspace boundary")
	}
}

func TestReconcileWorkspaceLabels_RequiresWorkspace(t *testing.T) {
	if _, err := ReconcileWorkspaceLabels(""); err == nil {
		t.Fatal("empty workspace must fail closed")
	}
}

// FAC-199 round-2 finding 3: ReconcileAgentLabel must fail closed when the
// agent inventory disagrees with the identity snapshot the caller renamed
// against — pane, session, cwd, or workspace changing between the rename
// decision and the post-rename readback is exactly the race a "read-verify
// before renaming already happened live" repair is supposed to catch.
func TestReconcileAgentLabel_FailsClosedWhenIdentityDriftsDuringRename(t *testing.T) {
	before := AgentEntry{Name: "forge-worker", TabID: "tab-x", PaneID: "pane-1", Workspace: "wK", Cwd: "/repo"}
	before.Session.Value = "sess-1"

	cases := []struct {
		name  string
		after AgentEntry
	}{
		{name: "pane_changed", after: func() AgentEntry { a := before; a.PaneID = "pane-2"; return a }()},
		{name: "session_changed", after: func() AgentEntry { a := before; a.Session.Value = "sess-2"; return a }()},
		{name: "cwd_changed", after: func() AgentEntry { a := before; a.Cwd = "/other"; return a }()},
		{name: "workspace_changed", after: func() AgentEntry { a := before; a.Workspace = "w1"; return a }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := runHerdr
			defer func() { runHerdr = old }()
			liveLabel := "task-fac-9-raw"
			runHerdr = func(args ...string) (string, error) {
				if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
					// ReconcileAgentLabel takes "before" as the caller's already-current
					// snapshot and only calls AgentList once, for the post-rename
					// readback — so this stub always answers with the drifted state.
					b, err := json.Marshal(struct {
						Result struct {
							Agents []AgentEntry `json:"agents"`
						} `json:"result"`
					}{Result: struct {
						Agents []AgentEntry `json:"agents"`
					}{Agents: []AgentEntry{tc.after}}})
					if err != nil {
						t.Fatal(err)
					}
					return string(b), nil
				}
				if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
					return fmt.Sprintf(`{"result":{"tabs":[{"tab_id":"tab-x","label":%q,"workspace_id":"wK"}]}}`, liveLabel), nil
				}
				if len(args) == 4 && args[0] == "tab" && args[1] == "rename" {
					liveLabel = args[3]
					return `{}`, nil
				}
				t.Fatalf("unexpected herdr call: %v", args)
				return "", nil
			}
			if _, renamed, err := ReconcileAgentLabel(before); err == nil {
				t.Fatalf("identity drift %s must fail closed, got renamed=%v", tc.name, renamed)
			}
		})
	}
}

func TestReconcileAgentLabel_RequiresTabID(t *testing.T) {
	if _, _, err := ReconcileAgentLabel(AgentEntry{Workspace: "wK"}); err == nil {
		t.Fatal("empty tab id must fail closed")
	}
}

// FAC-199: this is the mutation-killer for the resume/recovery call site —
// if ReconcileHerdforgeLabel stops being called from ResolveAgentTabWithDecision,
// renamedTo stays empty and the test fails. It also proves the repair is a
// pure in-place rename against independently-measured identity, not values
// the reconciliation code happens to already hold in memory:
//   - pane/session/generation are re-read from the same in-memory fixture the
//     production code reads from, and must be unchanged after the call;
//   - readPIDStartToken/readPIDParent (the only process-identity seam this
//     package has) must never be invoked — label repair never touches the
//     process tree;
//   - a real git repository's HEAD and working-tree dirty state are measured
//     with actual `git` commands before and after — since pkg/herdr never
//     shells out to git at all, this proves worktree state is physically
//     untouched, not merely assumed so from reading the diff;
//   - no "tab close" is ever issued.
func TestResumeReconcilesRawLabelInPlaceWithoutTabClose(t *testing.T) {
	// Hermetic git: ignore the developer/CI machine's global and system git
	// config (commit signing backends in particular — e.g. a 1Password SSH
	// signer with no agent socket available in a sandboxed test run) so this
	// fixture never depends on ambient environment.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "fac199@example.com")
	git("config", "user.name", "fac199")
	git("config", "commit.gpgsign", "false")
	git("config", "tag.gpgsign", "false")
	if err := os.WriteFile(repo+"/committed.txt", []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "committed.txt")
	git("commit", "-q", "-m", "init")
	if err := os.WriteFile(repo+"/dirty.txt", []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	headBefore := git("rev-parse", "HEAD")
	dirtyBefore := git("status", "--porcelain")

	req, lc, agent, _ := standingResumeFixture(t, false)

	oldToken, oldParent := readPIDStartToken, readPIDParent
	defer func() { readPIDStartToken, readPIDParent = oldToken, oldParent }()
	readPIDStartToken = func(int) (string, error) {
		t.Fatal("label reconciliation must never touch process identity (readPIDStartToken)")
		return "", nil
	}
	readPIDParent = func(int) (int, error) {
		t.Fatal("label reconciliation must never touch process identity (readPIDParent)")
		return 0, nil
	}

	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	var calls [][]string
	liveLabel := "task-fac-188-worker"
	var renamedTo string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch {
		case len(args) == 2 && args[0] == "agent" && args[1] == "list":
			b, _ := json.Marshal(struct {
				Result struct {
					Agents []AgentEntry `json:"agents"`
				} `json:"result"`
			}{Result: struct {
				Agents []AgentEntry `json:"agents"`
			}{Agents: []AgentEntry{agent}}})
			return string(b), nil
		case len(args) == 2 && args[0] == "tab" && args[1] == "list":
			return fmt.Sprintf(`{"result":{"tabs":[{"tab_id":%q,"label":%q,"workspace_id":%q}]}}`, agent.TabID, liveLabel, agent.Workspace), nil
		case len(args) == 4 && args[0] == "tab" && args[1] == "rename":
			renamedTo = args[3]
			liveLabel = args[3]
			return `{}`, nil
		default:
			t.Fatalf("unexpected herdr call (must not close/restart the tab): %v", args)
			return "", nil
		}
	}
	beforeGen := lc.Inventory.Owner.SessionGeneration
	beforePane := agent.PaneID
	beforeSession := agent.Session.Value
	if _, err := ResolveAgentTabWithDecision(agent.Name, req); err != nil {
		t.Fatal(err)
	}
	if renamedTo != "Herdforge · task-fac-188-worker" {
		t.Fatalf("raw label was not reconciled on resume: renamed to %q", renamedTo)
	}
	if lc.Inventory.Owner.SessionGeneration != beforeGen || agent.PaneID != beforePane || agent.Session.Value != beforeSession {
		t.Fatal("label reconciliation must not mutate pane, session, or process identity")
	}
	for _, c := range calls {
		if len(c) >= 2 && c[0] == "tab" && c[1] == "close" {
			t.Fatal("label reconciliation must never close the tab")
		}
	}

	headAfter := git("rev-parse", "HEAD")
	dirtyAfter := git("status", "--porcelain")
	if headAfter != headBefore {
		t.Fatalf("worktree HEAD changed: before=%s after=%s", headBefore, headAfter)
	}
	if dirtyAfter != dirtyBefore {
		t.Fatalf("worktree dirty state changed: before=%q after=%q", dirtyBefore, dirtyAfter)
	}
}

func TestAgentStartBoundaryRejectsRawAndRequiresDecision(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		return "{}", nil
	}
	if err := AgentStart("raw", "codex", "pane", "--model", testWorkerModel); err == nil {
		t.Fatal("raw/bare AgentStart must fail closed")
	}
	if len(calls) != 0 {
		t.Fatalf("raw rejection invoked process API: %v", calls)
	}
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d, TaskRef: "FAC-188", Repository: "repo", Lane: "worker", SessionGeneration: 1, Scope: router.ScopeTask, LeaseGeneration: d.LeaseGeneration}
	if err := AgentStartWithDecision("worker", d.Harness, "pane", req); err == nil {
		t.Fatal("direct worker start without a prepared lifecycle must fail closed")
	}
	if len(calls) != 0 {
		t.Fatalf("unprepared start reached process API: %v", calls)
	}
}

func TestAgentStartRejectsUnboundPiDecisionBeforeProcess(t *testing.T) {
	defer SetOwnerBindTimingForTest(20*time.Millisecond, time.Millisecond)()
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: "FAC-UNBOUND", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	owner := toolchild.Identity{TabID: "tab-unbound", PaneID: "pane-unbound", Name: "worker-unbound", SessionGeneration: 1, LaunchID: launch.DecisionDigest(d), Repository: "repo", Role: launch.WorkerRole, Lane: "worker", TaskRef: d.TaskRef, Provider: d.Harness, ArgvDigest: launch.DecisionDigest(d), Argv: append([]string(nil), d.HarnessArgv...)}
	path := filepath.Join(t.TempDir(), "receipts.jsonl")
	lc := toolchild.NewLifecycle(owner, &toolchild.FakeTree{}, &toolchild.JSONLSink{Path: path})
	if err := lc.Provision(); err != nil {
		t.Fatal(err)
	}
	toolChildMu.Lock()
	toolChildByTab[owner.TabID] = lc
	toolChildByPane[owner.PaneID] = lc
	toolChildMu.Unlock()
	defer dropToolChild(owner.TabID, owner.PaneID)
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	startCalls := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "start" {
			startCalls++
			return `{}`, nil
		}
		if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
			return `{}`, nil
		}
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) == 4 && args[0] == "pane" {
			return `{"error":{"code":"pane_not_found"}}`, errors.New("exit status 1")
		}
		return `{}`, nil
	}
	req := launch.Request{Decision: d, TaskRef: d.TaskRef, Repository: owner.Repository, Lane: owner.Lane, SessionGeneration: 1, LeaseGeneration: d.LeaseGeneration, Scope: d.Scope}
	err = AgentStartWithDecision(owner.Name, d.Harness, owner.PaneID, req)
	if err == nil || !strings.Contains(err.Error(), "bound Pi harness session") {
		t.Fatalf("unbound start error = %v", err)
	}
	if startCalls != 0 {
		t.Fatalf("unbound decision reached process seam %d times", startCalls)
	}
}

func TestValidatePreparedPiStartRejectsSwappedBoundDecision(t *testing.T) {
	newDecision := func(ref string, generation int64, session string) *router.LaunchDecision {
		d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: ref, LeaseGeneration: generation, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
		if err != nil {
			t.Fatal(err)
		}
		bound, err := router.BindHarnessSession(d, session)
		if err != nil {
			t.Fatal(err)
		}
		return bound
	}
	one := newDecision("FAC-ONE", 7, filepath.Join(t.TempDir(), "one.jsonl"))
	owner := toolchild.Identity{TabID: "tab", PaneID: "pane", Name: "worker", SessionGeneration: 7, LaunchID: launch.DecisionDigest(one), Repository: "repo", Role: launch.WorkerRole, Lane: "worker", TaskRef: one.TaskRef, Provider: one.Harness, ArgvDigest: launch.DecisionDigest(one), Argv: append([]string(nil), one.HarnessArgv...)}
	lc := toolchild.NewLifecycle(owner, &toolchild.FakeTree{}, &toolchild.MemorySink{})
	exact := launch.Request{Decision: one, TaskRef: one.TaskRef, Repository: owner.Repository, Lane: owner.Lane, SessionGeneration: 7, LeaseGeneration: one.LeaseGeneration, Scope: one.Scope}
	if err := validatePreparedPiStart(owner.TabID, lc, owner.Name, owner.PaneID, exact); err != nil {
		t.Fatalf("exact prepared authority rejected: %v", err)
	}
	two := newDecision("FAC-TWO", 8, filepath.Join(t.TempDir(), "two.jsonl"))
	swapped := launch.Request{Decision: two, TaskRef: two.TaskRef, Repository: owner.Repository, Lane: owner.Lane, SessionGeneration: 8, LeaseGeneration: two.LeaseGeneration, Scope: two.Scope}
	if err := validatePreparedPiStart(owner.TabID, lc, owner.Name, owner.PaneID, swapped); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("swapped authority error = %v", err)
	}
}

func TestPreparedStartUsesPaneProcessInfoAndExactRoutedOwner(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/launch.jsonl")
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	defer SetPIDParentReader(func(int) (int, error) { return 500, nil })()
	defer SetPIDStartTokenReader(func(int) (string, error) { return "agent-start", nil })()
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	attestCalls := 0
	defer SetPiSessionRouteAttesterForTest(func(path string, argv []string) error {
		attestCalls++
		if path != d.HarnessSession || !equalArgs(argv, d.HarnessArgv) {
			t.Fatalf("attester path=%q argv=%q", path, argv)
		}
		return nil
	})()
	argvReads := 0
	defer SetPIDArgvReader(func(pid int) ([]string, error) {
		if pid != 501 {
			t.Fatalf("argv reader pid = %d, want 501", pid)
		}
		argvReads++
		return append([]string{"node", "/opt/lib/node_modules/@earendil-works/pi-coding-agent/dist/cli.js"}, d.HarnessArgv[1:]...), nil
	})()
	req := launch.Request{Decision: d, TaskRef: d.TaskRef, LeaseGeneration: d.LeaseGeneration, SessionGeneration: 42, Scope: d.Scope, Repository: "herdforge-test", Lane: "worker"}
	owner := toolchild.Identity{PID: 501, ParentPID: 500, StartToken: "agent-start", SessionGeneration: 42, LaunchID: launch.DecisionDigest(d), Repository: "herdforge-test", Role: launch.WorkerRole, Lane: "worker", SessionID: "session-1", PaneID: "pane-1", TabID: "tab-1", Provider: d.Harness, ArgvDigest: launch.DecisionDigest(d), Argv: append([]string(nil), d.HarnessArgv...)}
	tree := &toolchild.FakeTree{Nodes: map[int]toolchild.Node{501: {Identity: owner, ParentPID: 500}, 601: {Identity: toolchild.Identity{PID: 601, ParentPID: 501, StartToken: "child-start"}, ParentPID: 501}}}
	lc := toolchild.NewLifecycle(toolchild.Identity{}, tree, &toolchild.MemorySink{})
	restoreFactory := SetToolChildLifecycleFactory(func(launch.Request, string, string) (ToolChildLifecycle, error) { return lc, nil })
	defer restoreFactory()
	calledInfo := false
	listCalls := 0
	var startArgs []string
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "start" {
			startArgs = append([]string(nil), args...)
			return `{}`, nil
		}
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			listCalls++
			if listCalls < 3 {
				return `{"result":{"agents":[]}}`, nil
			}
			return fmt.Sprintf(`{"result":{"agents":[{"name":"worker","agent":"pi","agent_status":"working","pane_id":"pane-1","tab_id":"tab-1","agent_session":{"value":%q}}]}}`, d.HarnessSession), nil
		}
		if len(args) == 4 && args[0] == "pane" && args[1] == "process-info" {
			calledInfo = true
			return `{"result":{"process_info":{"foreground_processes":[{"pid":501,"name":"node","cwd":"/repo","argv":[]}]}}}`, nil
		}
		return `{}`, nil
	}
	if err := StartPreparedAgent("tab-1", "worker", d.Harness, "pane-1", req); err != nil {
		t.Fatal(err)
	}
	if d.HarnessSession == "" {
		t.Fatal("HarnessSession must be nonempty")
	}
	if !filepath.IsAbs(d.HarnessSession) {
		t.Fatalf("HarnessSession = %q, want absolute path", d.HarnessSession)
	}
	if len(d.HarnessArgv) == 0 || d.HarnessArgv[len(d.HarnessArgv)-1] != d.HarnessSession {
		t.Fatalf("HarnessSession = %q, want last HarnessArgv element %q", d.HarnessSession, d.HarnessArgv)
	}
	wantStart := append([]string{"agent", "start", "worker", "--kind", "pi", "--pane", "pane-1", "--"}, d.HarnessArgv[1:]...)
	if !equalArgs(startArgs, wantStart) {
		t.Fatalf("Herdr start argv = %q, want %q", startArgs, wantStart)
	}
	if listCalls < 3 {
		t.Fatalf("expected owner-bind poll retries before agent identity, listCalls=%d", listCalls)
	}
	defer dropToolChild("tab-1", "pane-1")
	if !calledInfo {
		t.Fatal("agent-list-only path did not query exact pane process-info")
	}
	if argvReads == 0 {
		t.Fatal("expected empty pane argv to hydrate via PID argv reader")
	}
	if attestCalls == 0 {
		t.Fatal("native Pi owner skipped session attestation")
	}
	if err := ReconcileToolChild("tab-1", "done"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(tree.Reaped) != 1 || tree.Reaped[0] != 601 {
		t.Fatalf("reaped=%v", tree.Reaped)
	}
}

func TestStartPreparedValidationFailureCompensatesProvisionedAuthority(t *testing.T) {
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/receipts.jsonl"
	lc := toolchild.NewLifecycle(toolchild.Identity{}, &toolchild.FakeTree{}, &toolchild.JSONLSink{Path: path})
	restore := SetToolChildLifecycleFactory(func(launch.Request, string, string) (ToolChildLifecycle, error) { return lc, nil })
	defer restore()
	old := runHerdr
	defer func() { runHerdr = old }()
	list := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			list++
			if list == 1 {
				return `{"result":{"agents":[{"name":"worker","agent":"codex","pane_id":"pane","tab_id":"tab","agent_session":{"value":"session"}}]}}`, nil
			}
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
			return `{}`, nil
		}
		if len(args) == 4 && args[0] == "pane" {
			return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
		}
		return `{}`, nil
	}
	req := launch.Request{Decision: d, TaskRef: d.TaskRef, LeaseGeneration: d.LeaseGeneration, Scope: d.Scope, Repository: "repo", Lane: "worker"}
	if err := StartPreparedAgent("tab", "worker", "wrong-provider", "pane", req); err == nil {
		t.Fatal("provider validation unexpectedly passed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if lc.Bound() {
		t.Fatal("failed pre-bind compensation retained a bound owner")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

type rollbackLifecycle struct {
	bound                                            bool
	events                                           *[]string
	beginErr, reconcileErr, invalidateErr, verifyErr error
}

type failingProvisionSink struct{}

func (failingProvisionSink) Write(toolchild.Receipt) error {
	return errors.New("injected provisional sink failure")
}

func TestProvisioningFailureClosesExactTabWithoutPublishingReservation(t *testing.T) {
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	oldFactory := newToolChildLifecycle
	defer func() { newToolChildLifecycle = oldFactory }()
	closed := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
			closed = true
			return `{}`, nil
		}
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) == 4 && args[0] == "pane" {
			return `{"error":{"code":"pane_not_found"}}`, errors.New("exit status 1")
		}
		return `{}`, nil
	}
	newToolChildLifecycle = func(req launch.Request, _ string, _ string) (ToolChildLifecycle, error) {
		return toolchild.NewLifecycle(toolchild.Identity{}, toolchild.SystemTree{}, failingProvisionSink{}), nil
	}
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d, TaskRef: "FAC-188", Repository: "repo", Lane: "worker", SessionGeneration: 42, LeaseGeneration: 7, Scope: router.ScopeTask}
	if err := StartPreparedAgent("tab-provision-fail", "worker", d.Harness, "pane-provision-fail", req); err == nil {
		t.Fatal("provisioning failure was accepted")
	}
	if !closed {
		t.Fatal("exact prepared tab was not closed")
	}
	if lifecycleForTab("tab-provision-fail") != nil || lifecycleForPane("pane-provision-fail") != nil {
		t.Fatal("failed provisioning leaked map reservation")
	}
}

func TestNameOnlyCompensationRejectsAmbiguousAgents(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	closeCalls := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","pane_id":"p1","tab_id":"t1"},{"name":"worker","pane_id":"p2","tab_id":"t2"}]}}`, nil
		}
		if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
			closeCalls++
			return `{}`, nil
		}
		return `{}`, nil
	}
	if err := compensateStartedProcess("worker"); err == nil {
		t.Fatal("ambiguous name-only compensation was accepted")
	}
	if closeCalls != 0 {
		t.Fatal("ambiguous compensation closed a tab")
	}
}

func (l *rollbackLifecycle) Bind(toolchild.Identity) error { l.bound = true; return nil }
func (l *rollbackLifecycle) Bound() bool                   { return l.bound }
func (l *rollbackLifecycle) Begin() error                  { return l.beginErr }
func (l *rollbackLifecycle) Reconcile(string) error {
	*l.events = append(*l.events, "reconcile")
	return l.reconcileErr
}
func (l *rollbackLifecycle) Invalidate(string) error {
	*l.events = append(*l.events, "tombstone")
	return l.invalidateErr
}
func (l *rollbackLifecycle) VerifyTerminal() error {
	*l.events = append(*l.events, "terminal-readback")
	return l.verifyErr
}

func TestRollbackCrashMatrixRetainsAuthorityUntilTerminalTombstone(t *testing.T) {
	cases := []struct {
		name                                             string
		bound                                            bool
		reconcileErr, closeErr, invalidateErr, verifyErr error
		keepLive                                         bool
		wantErr, wantRetained                            bool
	}{
		{name: "after-herdr-start"},
		{name: "after-bind-failure"},
		{name: "after-begin-failure", bound: true, reconcileErr: errors.New("begin failure"), wantErr: true, wantRetained: true},
		{name: "after-launch-receipt-failure", bound: true},
		{name: "after-child-reconcile", bound: true, reconcileErr: errors.New("child reconcile crash"), wantErr: true, wantRetained: true},
		{name: "before-tab-close", closeErr: errors.New("close crash"), wantErr: true, wantRetained: true},
		{name: "after-close-before-readback", keepLive: true, wantErr: true, wantRetained: true},
		{name: "after-terminal-readback-before-tombstone", invalidateErr: errors.New("tombstone crash"), wantErr: true, wantRetained: true},
		{name: "after-tombstone", verifyErr: errors.New("terminal readback crash"), wantErr: true, wantRetained: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := runHerdr
			defer func() { runHerdr = old }()
			var events []string
			lc := &rollbackLifecycle{bound: tc.bound, events: &events, reconcileErr: tc.reconcileErr, invalidateErr: tc.invalidateErr, verifyErr: tc.verifyErr}
			toolChildMu.Lock()
			toolChildByPane["pane-crash"] = lc
			toolChildByTab["tab-crash"] = lc
			toolChildMu.Unlock()
			defer dropToolChild("tab-crash", "pane-crash")
			closeDone := false
			runHerdr = func(args ...string) (string, error) {
				if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
					return `{"result":{"tabs":[]}}`, nil
				}
				if len(args) == 4 && args[0] == "pane" && args[1] == "process-info" {
					return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
				}
				if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
					if tc.keepLive || !closeDone {
						return `{"result":{"agents":[{"name":"worker","pane_id":"pane-crash","tab_id":"tab-crash"}]}}`, nil
					}
					return `{"result":{"agents":[]}}`, nil
				}
				if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
					if tc.closeErr != nil {
						return "", tc.closeErr
					}
					closeDone = true
					return `{}`, nil
				}
				return `{}`, nil
			}
			err := rollbackToolChild("tab-crash", "pane-crash", lc, tc.name)
			if tc.wantErr != (err != nil) {
				t.Fatalf("error=%v wantErr=%v events=%v", err, tc.wantErr, events)
			}
			toolChildMu.Lock()
			_, retained := toolChildByTab["tab-crash"]
			toolChildMu.Unlock()
			if tc.wantRetained != retained {
				t.Fatalf("retained=%v want=%v events=%v", retained, tc.wantRetained, events)
			}
			if !tc.wantErr && (len(events) == 0 || events[len(events)-1] != "terminal-readback") {
				t.Fatalf("terminal evidence missing: %v", events)
			}
		})
	}
}

// This table enters the real StartPreparedAgent -> AgentStartWithDecision ->
// bind/Begin/receipt/rollback path. Herdr, process identity and lifecycle are
// all fake adapters; no host process or signal is involved.
func TestProductionStartCrashMatrixRetainsAuthorityUntilReadback(t *testing.T) {
	defer SetOwnerBindTimingForTest(200*time.Millisecond, 10*time.Millisecond)()
	cases := []struct {
		name                                                                          string
		startErr, bindErr, beginErr, reconcileErr, closeErr, invalidateErr, verifyErr bool
		keepLive                                                                      bool
	}{
		{name: "after-preparation-before-start", startErr: true},
		{name: "after-herdr-start-before-bind", bindErr: true},
		{name: "bind-failure", bindErr: true},
		{name: "begin-inventory-failure", beginErr: true},
		{name: "launch-receipt-failure"},
		{name: "child-reap-intent-result-failure", reconcileErr: true},
		{name: "before-raw-tab-close", closeErr: true},
		{name: "after-raw-tab-close-before-readback", keepLive: true},
		{name: "after-terminal-readback-before-tombstone", invalidateErr: true},
		{name: "after-tombstone-readback", verifyErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
			if err != nil {
				t.Fatal(err)
			}
			defer SetPiSessionRouteAttesterForTest(func(path string, argv []string) error {
				if path != d.HarnessSession || !equalArgs(argv, d.HarnessArgv) {
					return fmt.Errorf("%w: test route mismatch", ErrPiSessionRouteMismatch)
				}
				return nil
			})()
			var events []string
			lc := &rollbackLifecycle{events: &events}
			if tc.beginErr {
				lc.beginErr = errors.New("injected Begin failure")
			}
			if tc.reconcileErr {
				lc.reconcileErr = errors.New("injected reap intent/result failure")
			}
			if tc.invalidateErr {
				lc.invalidateErr = errors.New("injected tombstone boundary failure")
			}
			if tc.verifyErr {
				lc.verifyErr = errors.New("injected terminal readback failure")
			}
			restoreFactory := SetToolChildLifecycleFactory(func(launch.Request, string, string) (ToolChildLifecycle, error) { return lc, nil })
			defer restoreFactory()
			if !tc.startErr {
				t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
			} else {
				t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
			}
			oldRun := runHerdr
			defer func() { runHerdr = oldRun }()
			defer dropToolChild("tab-crash-prod", "pane-crash-prod")
			defer SetPIDParentReader(func(int) (int, error) { return 500, nil })()
			defer SetPIDStartTokenReader(func(int) (string, error) { return "start-agent", nil })()
			listCalls := 0
			closeDone := false
			runHerdr = func(args ...string) (string, error) {
				if len(args) >= 2 && args[0] == "agent" && args[1] == "start" && tc.startErr {
					return "", errors.New("injected Herdr start failure")
				}
				if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
					listCalls++
					if tc.keepLive || listCalls == 1 {
						return fmt.Sprintf(`{"result":{"agents":[{"name":"worker","agent":"pi","pane_id":"pane-crash-prod","tab_id":"tab-crash-prod","agent_session":{"value":%q}}]}}`, d.HarnessSession), nil
					}
					return `{"result":{"agents":[]}}`, nil
				}
				if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
					return `{"result":{"tabs":[]}}`, nil
				}
				if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
					if tc.closeErr {
						return "", errors.New("injected tab close failure")
					}
					closeDone = true
					return `{}`, nil
				}
				if len(args) == 4 && args[0] == "pane" && args[1] == "process-info" {
					if closeDone && !tc.keepLive {
						return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
					}
					if tc.bindErr {
						return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
					}
					piProcess := append([]string{"node", "/opt/lib/node_modules/@earendil-works/pi-coding-agent/dist/cli.js"}, d.HarnessArgv[1:]...)
					return fmt.Sprintf(`{"result":{"process_info":{"foreground_processes":[{"pid":501,"name":"node","argv":%s}]}}}`, mustJSON(piProcess)), nil
				}
				return `{}`, nil
			}
			req := launch.Request{Decision: d, TaskRef: d.TaskRef, LeaseGeneration: d.LeaseGeneration, SessionGeneration: 42, Scope: d.Scope, Repository: "repo", Lane: "worker"}
			if tc.name == "launch-receipt-failure" || tc.reconcileErr || tc.closeErr || tc.keepLive || tc.invalidateErr || tc.verifyErr {
				t.Setenv("HERD_LAUNCH_RECEIPTS", "/dev/null/receipts.jsonl")
			}
			err = StartPreparedAgent("tab-crash-prod", "worker", d.Harness, "pane-crash-prod", req)
			if err == nil {
				t.Fatal("crash boundary unexpectedly succeeded")
			}
			if tc.reconcileErr || tc.closeErr || tc.keepLive || tc.invalidateErr || tc.verifyErr {
				if lifecycleForTab("tab-crash-prod") == nil {
					t.Fatalf("authority dropped before verified terminal state: %v", events)
				}
			}
			if tc.invalidateErr || tc.verifyErr || tc.keepLive || tc.closeErr || tc.reconcileErr {
				if len(events) == 0 {
					t.Fatalf("authority events missing: %v", events)
				}
			}
		})
	}
}

func TestAgentStartRequiresExactClaimGenerationBeforeProcess(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "{}", nil
	}
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel,
		RequestedEffort: testWorkerEffort, TaskRef: "FAC-178", LeaseGeneration: 7,
		Scope:        router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, generation := range map[string]int64{"zero": 0, "mismatch": 6} {
		t.Run(name, func(t *testing.T) {
			before := len(calls)
			err := AgentStartWithDecision("worker", d.Harness, "pane", launch.Request{Decision: d, TaskRef: "FAC-178", Repository: "repo", Lane: "worker", SessionGeneration: 42, LeaseGeneration: generation, Scope: router.ScopeTask})
			if err == nil {
				t.Fatal("zero or mismatched generation must fail before process seam")
			}
			if len(calls) != before {
				t.Fatalf("rejected generation reached process seam: %v", calls[before:])
			}
		})
	}
	if err := AgentStartWithDecision("worker", d.Harness, "pane", launch.Request{Decision: d, TaskRef: "FAC-178", Repository: "repo", Lane: "worker", SessionGeneration: 42, LeaseGeneration: 7, Scope: router.ScopeTask}); err == nil {
		t.Fatal("exact generation without prepared lifecycle must still fail closed")
	}
	if len(calls) != 0 {
		t.Fatalf("unprepared exact-generation start reached process seam: %v", calls)
	}
}

func TestResumeUsesDurableClientIdentityNotHerdrMetadata(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	d, err = router.RebindDecision(d, "FAC-175", 7)
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d, TaskRef: "FAC-175", Name: "standing-worker", PaneID: "pane-1", LeaseGeneration: 7, Scope: router.ScopeTask}
	if err := launch.RecordStarted(req, nil); err != nil {
		t.Fatal(err)
	}
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"standing-worker","pane_id":"pane-1","tab_id":"tab-1"}]}}`, nil
		}
		return "{}", nil
	}
	if got, err := ResolveAgentTabWithDecision("standing-worker", launch.Request{Decision: d, TaskRef: "FAC-175", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask}); err == nil || got != "" {
		t.Fatalf("unbound provisional resume must fail closed: %q %v", got, err)
	}
	if _, err := ResolveAgentTabWithDecision("standing-worker", launch.Request{Decision: d, TaskRef: "other", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask}); err == nil {
		t.Fatal("different task identity must fail closed before resume")
	}
	if _, err := ResolveAgentTabWithDecision("missing", launch.Request{Decision: d, TaskRef: "FAC-175", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("expected typed not-found, got %v", err)
	}
}

func TestStandingReceiptCannotAuthorizeClaimedTaskAssignment(t *testing.T) {
	receiptPath := t.TempDir() + "/receipts.jsonl"
	t.Setenv("HERD_LAUNCH_RECEIPTS", receiptPath)
	standing, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel,
		RequestedEffort: testWorkerEffort, TaskRef: "worker",
		Scope:        router.ScopeLane,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launch.RecordStarted(launch.Request{Decision: standing, TaskRef: "worker", Name: "forge-worker", PaneID: "pane-1", LeaseGeneration: 0}, nil); err != nil {
		t.Fatal(err)
	}
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return `{"result":{"agents":[{"name":"forge-worker","pane_id":"pane-1","tab_id":"tab-1"}]}}`, nil
	}
	for _, tc := range []struct {
		name string
		ref  string
		gen  int64
	}{
		{name: "FAC-A", ref: "FAC-A", gen: 7},
		{name: "FAC-B", ref: "FAC-B", gen: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := router.RebindDecision(standing, tc.ref, tc.gen)
			if err != nil {
				t.Fatal(err)
			}
			before := len(calls)
			_, err = ResolveAgentTabWithDecision("forge-worker", launch.Request{Decision: decision, TaskRef: tc.ref, LeaseGeneration: tc.gen, Scope: router.ScopeTask})
			if !errors.Is(err, ErrAgentIdentityMismatch) {
				t.Fatalf("lane receipt must not authorize %s/g%d: %v", tc.ref, tc.gen, err)
			}
			if len(calls) != before+1 || calls[before][0] != "agent" || calls[before][1] != "list" {
				t.Fatalf("assignment rejection must inspect only the live agent: %v", calls[before:])
			}
		})
	}
}

func TestResumeRejectsStoredCoordinatorTierDecisionWithoutPrompt(t *testing.T) {
	receiptPath := t.TempDir() + "/receipts.jsonl"
	t.Setenv("HERD_LAUNCH_RECEIPTS", receiptPath)
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	d, err = router.RebindDecision(d, "FAC-175", 7)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := &router.LaunchDecision{Role: router.RoleWorker, Shape: launch.Implementation, Provider: testWorkerProvider, Model: "gpt-5.6-sol", Effort: "ultra", Argv: []string{"codex", "--model", "gpt-5.6-sol", "-c", "model_reasoning_effort=ultra", "-a", "never"}}
	historical := launch.Receipt{TaskRef: "FAC-175", Role: launch.WorkerRole, TaskShape: launch.Implementation, Provider: testWorkerProvider, Model: forbidden.Model, Effort: forbidden.Effort, DecisionDigest: launch.DecisionDigest(forbidden), Argv: forbidden.Argv, Accepted: true, Name: "stored-worker", PaneID: "pane-1", LeaseGeneration: 7}
	if err := (&launch.JSONLSink{Path: receiptPath}).Write(historical); err != nil {
		t.Fatal(err)
	}
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return `{"result":{"agents":[{"name":"stored-worker","pane_id":"pane-1","tab_id":"tab-1"}]}}`, nil
	}
	_, err = ResolveAgentTabWithDecision("stored-worker", launch.Request{Decision: d, TaskRef: "FAC-175", Name: "stored-worker", PaneID: "pane-1", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask})
	if !errors.Is(err, ErrAgentIdentityMismatch) {
		t.Fatalf("stored Sol/Ultra session must be blocked by durable identity mismatch, got %v", err)
	}
	if len(calls) != 1 || calls[0][0] != "agent" || calls[0][1] != "list" {
		t.Fatalf("blocked resume must only inspect the live agent: %v", calls)
	}
}

func TestResumePreservesMalformedCurrentDecisionError(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	d, err = router.RebindDecision(d, "FAC-175", 7)
	if err != nil {
		t.Fatal(err)
	}
	d.Model = "gpt-5.6-sol"
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls int
	runHerdr = func(args ...string) (string, error) {
		calls++
		return `{}`, nil
	}
	_, err = ResolveAgentTabWithDecision("stored-worker", launch.Request{Decision: d, TaskRef: "FAC-175", Name: "stored-worker", PaneID: "pane-1", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask})
	if err == nil || errors.Is(err, ErrAgentIdentityMismatch) {
		t.Fatalf("malformed current decision must preserve validation error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("malformed current decision must not inspect a live agent: %d calls", calls)
	}
}

func TestResumeRejectsMissingAndStaleReceiptsWithoutProcessOrPrompt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		populate func(string) error
	}{
		{name: "missing", populate: func(string) error { return nil }},
		{name: "lease-mismatch", populate: func(path string) error {
			d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
			if err != nil {
				return err
			}
			d, err = router.RebindDecision(d, "FAC-175", 6)
			if err != nil {
				return err
			}
			return (&launch.JSONLSink{Path: path}).Write(launch.Receipt{TaskRef: "FAC-175", Role: launch.WorkerRole, TaskShape: launch.Implementation, Provider: testWorkerProvider, Model: testWorkerModel, Effort: testWorkerEffort, DecisionDigest: launch.DecisionDigest(d), Argv: d.Argv, Accepted: true, Name: "stored-worker", PaneID: "pane-1", LeaseGeneration: 6})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/receipts.jsonl"
			t.Setenv("HERD_LAUNCH_RECEIPTS", path)
			if err := tc.populate(path); err != nil {
				t.Fatal(err)
			}
			d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
			if err != nil {
				t.Fatal(err)
			}
			d, err = router.RebindDecision(d, "FAC-175", 7)
			if err != nil {
				t.Fatal(err)
			}
			old := runHerdr
			defer func() { runHerdr = old }()
			var calls [][]string
			runHerdr = func(args ...string) (string, error) {
				calls = append(calls, append([]string(nil), args...))
				return `{"result":{"agents":[{"name":"stored-worker","pane_id":"pane-1","tab_id":"tab-1"}]}}`, nil
			}
			_, err = ResolveAgentTabWithDecision("stored-worker", launch.Request{Decision: d, TaskRef: "FAC-175", Name: "stored-worker", PaneID: "pane-1", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask})
			if !errors.Is(err, ErrAgentIdentityMismatch) {
				t.Fatalf("%s receipt must be blocked, got %v", tc.name, err)
			}
			if len(calls) != 1 || calls[0][0] != "agent" || calls[0][1] != "list" {
				t.Fatalf("%s resume must not start or prompt: %v", tc.name, calls)
			}
		})
	}
}

func standingResumeFixture(t *testing.T, durable bool) (launch.Request, *toolchild.Lifecycle, AgentEntry, string) {
	t.Helper()
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/launch.jsonl")
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation, TaskRef: "FAC-188",
		LeaseGeneration: 7, RequestedProvider: testWorkerProvider,
		RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort,
		Scope:        router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := router.BindHarnessSession(d, filepath.Join(t.TempDir(), "standing.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	d = bound
	restoreAttester := SetPiSessionRouteAttesterForTest(func(path string, argv []string) error {
		if path != d.HarnessSession || !equalArgs(argv, d.HarnessArgv) {
			return fmt.Errorf("%w: standing route mismatch", ErrPiSessionRouteMismatch)
		}
		return nil
	})
	t.Cleanup(restoreAttester)
	req := launch.Request{Decision: d, TaskRef: "FAC-188", Name: "forge-worker", PaneID: "pane-standing", LeaseGeneration: 7, Scope: router.ScopeTask, Repository: "repo-standing", Lane: "worker"}
	digest := launch.DecisionDigest(d)
	owner := toolchild.Identity{PID: 900, StartToken: "owner-start", SessionGeneration: 42, LaunchID: digest, Repository: req.Repository, Role: string(d.Role), Lane: req.Lane, SessionID: d.HarnessSession, PaneID: req.PaneID, TabID: "tab-standing", Provider: d.Harness, ArgvDigest: digest, Argv: append([]string(nil), d.HarnessArgv...), TaskRef: req.TaskRef, Name: req.Name}
	lc := toolchild.NewLifecycle(owner, &toolchild.FakeTree{}, &toolchild.MemorySink{})
	path := t.TempDir() + "/toolchild.jsonl"
	if durable {
		sink := &toolchild.JSONLSink{Path: path}
		if err := sink.Write(toolchild.Receipt{Action: "owner", Identity: owner, Reason: "exact launch owner bound"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HERD_TOOLCHILD_RECEIPTS", path)
	} else {
		toolChildMu.Lock()
		toolChildByTab[owner.TabID] = lc
		toolChildByPane[owner.PaneID] = lc
		toolChildMu.Unlock()
		t.Cleanup(func() { dropToolChild(owner.TabID, owner.PaneID) })
	}
	started := req
	started.SessionGeneration = owner.SessionGeneration
	if err := launch.RecordStarted(started, nil); err != nil {
		t.Fatal(err)
	}
	agent := AgentEntry{Name: owner.Name, Kind: owner.Provider, Status: "working", PaneID: owner.PaneID, TabID: owner.TabID, Workspace: "wK"}
	agent.Session.Value = owner.SessionID
	req.SessionGeneration = 0
	return req, lc, agent, path
}

func TestStandingResumeRequiresExactSessionAttestation(t *testing.T) {
	req, _, agent, _ := standingResumeFixture(t, false)
	restore := SetPiSessionRouteAttesterForTest(func(string, []string) error { return fmt.Errorf("%w: injected mismatch", ErrPiSessionRouteMismatch) })
	defer restore()
	if _, err := recoverStandingLifecycle(agent, req); !errors.Is(err, ErrAgentIdentityMismatch) || !errors.Is(err, ErrPiSessionRouteMismatch) {
		t.Fatalf("standing session mismatch error = %v", err)
	}
}

func TestStandingResumeRecoversGenerationFromTaskLaunchRequestShape(t *testing.T) {
	req, lc, agent, _ := standingResumeFixture(t, false)
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			b, _ := json.Marshal(struct {
				Result struct {
					Agents []AgentEntry `json:"agents"`
				} `json:"result"`
			}{Result: struct {
				Agents []AgentEntry `json:"agents"`
			}{Agents: []AgentEntry{agent}}})
			return string(b), nil
		}
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return fmt.Sprintf(`{"result":{"tabs":[{"tab_id":%q,"label":"Herdforge · forge-worker","workspace_id":%q}]}}`, agent.TabID, agent.Workspace), nil
		}
		return "", fmt.Errorf("unexpected process or tab side effect: %v", args)
	}
	if _, err := ResolveAgentTabWithDecision(agent.Name, req); err != nil {
		t.Fatal(err)
	}
	if lc.Inventory.Owner.SessionGeneration != 42 || len(calls) != 2 {
		t.Fatalf("standing lane was not reused exactly: generation=%d calls=%v", lc.Inventory.Owner.SessionGeneration, calls)
	}
}

func TestStandingResumeRecoversGenerationAfterCoordinatorRestart(t *testing.T) {
	req, _, agent, path := standingResumeFixture(t, true)
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	toolChildMu.Lock()
	toolChildByTab = map[string]ToolChildLifecycle{}
	toolChildByPane = map[string]ToolChildLifecycle{}
	toolChildMu.Unlock()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return fmt.Sprintf(`{"result":{"agents":[{"name":%q,"agent":%q,"agent_status":"working","pane_id":%q,"tab_id":%q,"workspace_id":%q,"agent_session":{"value":%q}}]}}`, agent.Name, agent.Kind, agent.PaneID, agent.TabID, agent.Workspace, agent.Session.Value), nil
		}
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return fmt.Sprintf(`{"result":{"tabs":[{"tab_id":%q,"label":"Herdforge · forge-worker","workspace_id":%q}]}}`, agent.TabID, agent.Workspace), nil
		}
		return "", fmt.Errorf("unexpected process or tab side effect: %v", args)
	}
	if _, err := ResolveAgentTabWithDecision(agent.Name, req); err != nil {
		t.Fatalf("restart recovery from %s failed: %v", path, err)
	}
	if lifecycleForTab(agent.TabID) == nil {
		t.Fatal("restart recovery did not retain exact lifecycle authority")
	}
}

func TestStandingResumeRejectsLifecycleTupleMismatches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*toolchild.Identity, *AgentEntry, *launch.Request)
	}{
		{name: "repository", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.Repository = "other" }},
		{name: "task", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.TaskRef = "FAC-other" }},
		{name: "lane", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.Lane = "other" }},
		{name: "provider", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.Provider = "other" }},
		{name: "digest", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.LaunchID = "other" }},
		{name: "argv", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.Argv = []string{"codex", "mutated"} }},
		{name: "session", mutate: func(_ *toolchild.Identity, a *AgentEntry, _ *launch.Request) { a.Session.Value = "other" }},
		{name: "pane", mutate: func(o *toolchild.Identity, a *AgentEntry, _ *launch.Request) { o.PaneID, a.PaneID = "other", "other" }},
		{name: "generation", mutate: func(o *toolchild.Identity, _ *AgentEntry, r *launch.Request) {
			r.SessionGeneration = o.SessionGeneration + 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, lc, agent, _ := standingResumeFixture(t, false)
			tc.mutate(&lc.Inventory.Owner, &agent, &req)
			oldRun := runHerdr
			defer func() { runHerdr = oldRun }()
			runHerdr = func(args ...string) (string, error) {
				if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
					return fmt.Sprintf(`{"result":{"agents":[{"name":%q,"agent":%q,"agent_status":"working","pane_id":%q,"tab_id":%q,"agent_session":{"value":%q}}]}}`, agent.Name, agent.Kind, agent.PaneID, agent.TabID, agent.Session.Value), nil
				}
				return "", fmt.Errorf("unexpected side effect: %v", args)
			}
			if _, err := ResolveAgentTabWithDecision(agent.Name, req); !errors.Is(err, ErrAgentIdentityMismatch) {
				t.Fatalf("mismatch %s was not fail-closed: %v", tc.name, err)
			}
		})
	}
}

func TestReceiptFailureClosesAndVerifiesExactTab(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", "/dev/null/launch-receipts.jsonl")
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	old := runHerdr
	defer func() { runHerdr = old }()
	listCalls := 0
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			listCalls++
			if listCalls == 1 {
				return `{"result":{"agents":[{"name":"worker","pane_id":"pane","tab_id":"tab"}]}}`, nil
			}
			return `{"result":{"agents":[]}}`, nil
		}
		return "{}", nil
	}
	if err := AgentStartWithDecision("worker", d.Harness, "pane", launch.Request{Decision: d, TaskRef: "FAC-188", Repository: "repo", Lane: "worker", SessionGeneration: 42, LeaseGeneration: 7, Scope: router.ScopeTask}); err == nil || !strings.Contains(err.Error(), "prepared tool-child lifecycle") {
		t.Fatalf("unprepared receipt boundary must fail before process API: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("unprepared receipt path reached process API: %v", calls)
	}
}

func TestBindToolChildLifecycleRejectsHarnessMismatch(t *testing.T) {
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	decision, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel,
		RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-MISMATCH",
		LeaseGeneration: 7, Scope: router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: decision, TaskRef: "FAC-MISMATCH", LeaseGeneration: 7, SessionGeneration: 1, Scope: router.ScopeTask, Repository: "repo", Lane: "worker"}
	lc := toolchild.NewLifecycle(toolchild.Identity{}, &toolchild.FakeTree{}, &toolchild.MemorySink{})
	toolChildMu.Lock()
	toolChildByPane["pane-mismatch"] = lc
	toolChildByTab["tab-mismatch"] = lc
	toolChildMu.Unlock()
	defer dropToolChild("tab-mismatch", "pane-mismatch")

	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	processCalls := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker-mismatch","agent":"codex","agent_status":"working","pane_id":"pane-mismatch","tab_id":"tab-mismatch","agent_session":{"value":"session-1"}}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			processCalls++
		}
		return `{}`, nil
	}

	err = bindToolChildLifecycle("pane-mismatch", "worker-mismatch", req)
	if err == nil || !strings.Contains(err.Error(), "harness mismatch") || !strings.Contains(err.Error(), "got codex want pi") {
		t.Fatalf("expected exact harness mismatch, got %v", err)
	}
	if processCalls != 0 {
		t.Fatalf("harness mismatch reached process inspection: %d", processCalls)
	}
}

func TestNativeCandidatePiRequiresExactHarnessArgvAndEntrypoint(t *testing.T) {
	routed := []string{"pi", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium"}
	cases := []struct {
		name string
		p    PaneProcess
		want bool
	}{
		{"direct pi", PaneProcess{Name: "pi", Argv: []string{"/usr/local/bin/pi", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium"}}, true},
		{"node package cli", PaneProcess{Name: "node", Argv: []string{"/usr/local/bin/node", "/opt/lib/node_modules/@earendil-works/pi-coding-agent/dist/cli.js", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium"}}, true},
		{"node pi symlink", PaneProcess{Name: "node", Argv: []string{"node", "/usr/local/bin/pi", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium"}}, true},
		{"extra arg", PaneProcess{Name: "pi", Argv: []string{"pi", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium", "--verbose"}}, false},
		{"missing thinking", PaneProcess{Name: "pi", Argv: []string{"pi", "--model", "openai-codex/gpt-5.6-luna"}}, false},
		{"wrong model", PaneProcess{Name: "pi", Argv: []string{"pi", "--model", "openai-codex/gpt-5.6-sol", "--thinking", "medium"}}, false},
		{"unrelated node cli", PaneProcess{Name: "node", Argv: []string{"node", "/tmp/other/cli.js", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium"}}, false},
		{"unrelated node", PaneProcess{Name: "node", Argv: []string{"node", "/opt/server.js"}}, false},
		{"titled node", PaneProcess{Name: "node", Argv: []string{"pi", "", ""}}, false},
		{"name pi wrong argv0", PaneProcess{Name: "pi", Argv: []string{"/tmp/evil", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeCandidate(router.PiHarness, routed, tc.p); got != tc.want {
				t.Fatalf("nativeCandidate=%v want %v for %#v", got, tc.want, tc.p)
			}
		})
	}
}

func TestBindToolChildLifecycleTimeoutReportsObservedPiProcess(t *testing.T) {
	defer SetOwnerBindTimingForTest(30*time.Millisecond, time.Millisecond)()
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-DIAG", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d, TaskRef: d.TaskRef, LeaseGeneration: d.LeaseGeneration, SessionGeneration: 1, Scope: d.Scope, Repository: "repo", Lane: "worker"}
	lc := toolchild.NewLifecycle(toolchild.Identity{}, &toolchild.FakeTree{}, &toolchild.MemorySink{})
	toolChildMu.Lock()
	toolChildByPane["pane-diag"] = lc
	toolChildByTab["tab-diag"] = lc
	toolChildMu.Unlock()
	defer dropToolChild("tab-diag", "pane-diag")
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker-diag","agent":"pi","agent_status":"starting","pane_id":"pane-diag","tab_id":"tab-diag","agent_session":{"value":"session-diag"}}]}}`, nil
		}
		if len(args) == 4 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"foreground_processes":[{"pid":777,"name":"pi","argv":["pi","--model","openai-codex/gpt-5.6-sol","--thinking","medium"]}]}}}`, nil
		}
		return `{}`, nil
	}
	err = bindToolChildLifecycle("pane-diag", "worker-diag", req)
	if err == nil || !strings.Contains(err.Error(), "process candidates exact=0 wrappers=0 total=1") || !strings.Contains(err.Error(), "openai-codex/gpt-5.6-sol") {
		t.Fatalf("timeout diagnostics missing observed process: %v", err)
	}
}

func TestBindToolChildLifecycleTimeoutReportsArgvReadError(t *testing.T) {
	defer SetOwnerBindTimingForTest(30*time.Millisecond, time.Millisecond)()
	defer SetPIDArgvReader(func(pid int) ([]string, error) { return nil, fmt.Errorf("argv denied for %d", pid) })()
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-ARGV", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d, TaskRef: d.TaskRef, LeaseGeneration: d.LeaseGeneration, SessionGeneration: 1, Scope: d.Scope, Repository: "repo", Lane: "worker"}
	lc := toolchild.NewLifecycle(toolchild.Identity{}, &toolchild.FakeTree{}, &toolchild.MemorySink{})
	toolChildMu.Lock()
	toolChildByPane["pane-argv"] = lc
	toolChildByTab["tab-argv"] = lc
	toolChildMu.Unlock()
	defer dropToolChild("tab-argv", "pane-argv")
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker-argv","agent":"pi","agent_status":"starting","pane_id":"pane-argv","tab_id":"tab-argv","agent_session":{"value":"session-argv"}}]}}`, nil
		}
		if len(args) == 4 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"foreground_processes":[{"pid":777,"name":"node","argv":[]}]}}}`, nil
		}
		return `{}`, nil
	}
	err = bindToolChildLifecycle("pane-argv", "worker-argv", req)
	for _, want := range []string{"process argv unavailable", "pid 777 argv", "argv denied for 777", `name="node" argv=[]`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("argv read timeout error %v missing %q", err, want)
		}
	}
}

func TestRoutedProcessCandidatesRequiresPiSessionAttestationForNativePi(t *testing.T) {
	routed := []string{"pi", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium", "--session", "/sessions/signed.jsonl"}
	processes := []PaneProcess{{PID: 42, Name: "node", Argv: append([]string(nil), routed...)}}
	t.Run("matching signed session", func(t *testing.T) {
		called := 0
		defer SetPiSessionRouteAttesterForTest(func(path string, argv []string) error {
			called++
			if path != routed[6] || !equalArgs(argv, routed) {
				t.Fatalf("attester path=%q argv=%q", path, argv)
			}
			return nil
		})()
		matches, wrappers, wait, err := routedProcessCandidates(router.PiHarness, routed, routed[6], processes)
		if err != nil || wait != "" || called != 1 || len(matches) != 1 || matches[0].PID != 42 || len(wrappers) != 0 {
			t.Fatalf("matches=%v wrappers=%v wait=%q err=%v called=%d", matches, wrappers, wait, err, called)
		}
	})
	t.Run("reported session differs from signed argv", func(t *testing.T) {
		defer SetPiSessionRouteAttesterForTest(func(path string, argv []string) error {
			if path != routed[6] {
				return fmt.Errorf("%w: bound session path mismatch", ErrPiSessionRouteMismatch)
			}
			return nil
		})()
		matches, _, wait, err := routedProcessCandidates(router.PiHarness, routed, "/sessions/other.jsonl", processes)
		if !errors.Is(err, ErrPiSessionRouteMismatch) || len(matches) != 0 || wait != "" {
			t.Fatalf("matches=%v wait=%q err=%v", matches, wait, err)
		}
	})
}

func TestRoutedProcessCandidatesRequiresPiSessionAttestationForTitledNode(t *testing.T) {
	routed := []string{"pi", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium"}
	processes := []PaneProcess{{PID: 42, Name: "node", Argv: []string{"pi", "", "", "", "", ""}}}
	called := 0
	defer SetPiSessionRouteAttesterForTest(func(path string, argv []string) error {
		called++
		if path != "/sessions/exact.jsonl" || !equalArgs(argv, routed) {
			t.Fatalf("attester path=%q argv=%q", path, argv)
		}
		return nil
	})()
	matches, wrappers, wait, err := routedProcessCandidates("pi", routed, "/sessions/exact.jsonl", processes)
	if err != nil || wait != "" || called != 1 || len(matches) != 1 || matches[0].PID != 42 || len(wrappers) != 0 {
		t.Fatalf("matches=%v wrappers=%v wait=%q err=%v called=%d", matches, wrappers, wait, err, called)
	}
}

func TestRoutedProcessCandidatesFailsClosedOnPiSessionAttestation(t *testing.T) {
	routed := []string{"pi", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium"}
	processes := []PaneProcess{{PID: 42, Name: "node", Argv: []string{"pi", "", "", "", "", ""}}}
	t.Run("not ready waits without candidate", func(t *testing.T) {
		defer SetPiSessionRouteAttesterForTest(func(string, []string) error { return fmt.Errorf("%w: incomplete", ErrPiSessionNotReady) })()
		matches, _, wait, err := routedProcessCandidates("pi", routed, "/sessions/new.jsonl", processes)
		if err != nil || len(matches) != 0 || !strings.Contains(wait, ErrPiSessionNotReady.Error()) {
			t.Fatalf("matches=%v wait=%q err=%v", matches, wait, err)
		}
	})
	t.Run("mismatch rejects", func(t *testing.T) {
		defer SetPiSessionRouteAttesterForTest(func(string, []string) error { return fmt.Errorf("%w: wrong model", ErrPiSessionRouteMismatch) })()
		matches, _, wait, err := routedProcessCandidates("pi", routed, "/sessions/wrong.jsonl", processes)
		if !errors.Is(err, ErrPiSessionRouteMismatch) || len(matches) != 0 || wait != "" {
			t.Fatalf("matches=%v wait=%q err=%v", matches, wait, err)
		}
	})
}

func TestPiProcessTitleCandidateIsNarrow(t *testing.T) {
	if !piProcessTitleCandidate(PaneProcess{Name: "node", Argv: []string{"pi", "", ""}}) {
		t.Fatal("exact node Pi title rejected")
	}
	for _, p := range []PaneProcess{
		{Name: "pi", Argv: []string{"pi", "", ""}},
		{Name: "node", Argv: []string{"pi", "--model", ""}},
		{Name: "node", Argv: []string{"other", "", ""}},
		{Name: "node", Argv: nil},
	} {
		if piProcessTitleCandidate(p) {
			t.Fatalf("broad Pi title candidate accepted: %#v", p)
		}
	}
}
