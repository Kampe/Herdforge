package harvest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

type capacityBehaviorCommands struct {
	branchOutputs []string
	dirLog        string
	mu            sync.Mutex
	calls         []string
	root          string
	wts           []string
	order         *capacityBehaviorTimeline
	retargetFrom  string
	retargetTo    string
	retargeted    bool
}

type capacityBehaviorTimeline struct {
	mu      sync.Mutex
	entries []string
}

func (t *capacityBehaviorTimeline) add(entry string) {
	t.mu.Lock()
	t.entries = append(t.entries, entry)
	t.mu.Unlock()
}

func (t *capacityBehaviorTimeline) index(prefix string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, entry := range t.entries {
		if strings.HasPrefix(entry, prefix) {
			return i
		}
	}
	return -1
}

func (c *capacityBehaviorCommands) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	c.mu.Lock()
	c.calls = append(c.calls, strings.Join(args, " "))
	output := ""
	if len(args) >= 3 && args[0] == "worktree" && args[1] == "list" {
		lines := []string{"worktree " + c.root}
		for _, wt := range c.wts {
			lines = append(lines, "worktree "+wt)
		}
		output = strings.Join(lines, "\n") + "\n"
	} else if len(args) >= 1 && args[0] == "rev-parse" {
		output = "feature\n"
		if len(c.branchOutputs) > 0 {
			output = c.branchOutputs[0] + "\n"
			c.branchOutputs = c.branchOutputs[1:]
		}
	}
	c.mu.Unlock()
	if len(args) > 0 && args[0] == "rev-parse" && c.retargetFrom != "" && !c.retargeted {
		c.retargeted = true
		_ = os.Remove(c.retargetFrom)
		_ = os.Symlink(c.retargetTo, c.retargetFrom)
	}
	if c.order != nil && len(args) > 0 {
		c.order.add("git:" + args[0])
	}
	if len(args) > 0 && args[0] == "rev-parse" && c.retargetFrom != "" && !c.retargeted {
		c.retargeted = true
		_ = os.Remove(c.retargetFrom)
		_ = os.Symlink(c.retargetTo, c.retargetFrom)
	}
	if c.dirLog != "" && len(args) > 0 && (args[0] == "rev-parse" || args[0] == "fetch" || args[0] == "cherry") {
		quotedLog := "'" + strings.ReplaceAll(c.dirLog, "'", "'\\''") + "'"
		script := "pwd >> " + quotedLog
		if args[0] == "rev-parse" {
			script += "; printf 'feature\\n'"
		}
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
	return exec.CommandContext(ctx, "printf", "%s", output)
}

func (c *capacityBehaviorCommands) count(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, call := range c.calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

func (c *capacityBehaviorCommands) index(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, call := range c.calls {
		if strings.HasPrefix(call, prefix) {
			return i
		}
	}
	return -1
}

type capacityBehaviorAdmission struct {
	retargetFrom   string
	retargetTo     string
	mu             sync.Mutex
	stage          string
	events         []string
	plans          int
	batchPlans     int
	admittedScopes []string
	order          *capacityBehaviorTimeline
}

func (a *capacityBehaviorAdmission) Admit(resources.DiskRequest) resources.DiskDecision {
	return resources.DiskDecision{Allowed: false, State: resources.DiskBlocked, Evidence: resources.DiskEvidence{
		Reason: resources.DiskReasonInvalid, NextAction: resources.DiskActionFixPolicy,
	}}
}

func (a *capacityBehaviorAdmission) PlanDiskAdmission(requests []resources.DiskRequest) (resources.DiskAdmissionPlan, error) {
	a.mu.Lock()
	a.plans++
	a.events = append(a.events, "plan")
	if a.order != nil {
		a.order.add("plan")
	}
	stage := a.stage
	a.mu.Unlock()
	if stage == "plan" {
		return resources.DiskAdmissionPlan{}, &resources.DiskAdmissionError{Scope: "plan", Decision: resources.DiskDecision{
			State:    resources.DiskBlocked,
			Evidence: resources.DiskEvidence{Kind: "disk_pressure", Reason: resources.DiskReasonUnavailable, Operation: "harvest_batch", NextAction: resources.DiskActionRetryProbe},
		}}
	}
	plan := resources.DiskAdmissionPlan{Requests: make([]resources.DiskRequest, len(requests)), Scopes: make([]string, len(requests))}
	copy(plan.Requests, requests)
	for i := range plan.Requests {
		plan.Scopes[i] = "scope-" + string(rune('a'+i))
	}
	return plan, nil
}

func (a *capacityBehaviorAdmission) AdmitDiskPlan(plan resources.DiskAdmissionPlan) error {
	a.mu.Lock()
	a.batchPlans++
	a.events = append(a.events, "admit")
	if a.order != nil {
		a.order.add("admit")
	}
	stage := a.stage
	if stage != "admit" {
		for _, scope := range plan.Scopes {
			a.admittedScopes = append(a.admittedScopes, scope)
			a.events = append(a.events, "admit:"+scope)
			if a.order != nil {
				a.order.add("admit:" + scope)
			}
		}
	}
	a.mu.Unlock()
	if stage == "admit" {
		return &resources.DiskAdmissionError{Scope: plan.Scopes[0], Decision: resources.DiskDecision{
			State:    resources.DiskBlocked,
			Evidence: resources.DiskEvidence{Kind: "disk_pressure", Reason: resources.DiskReasonBelowThreshold, Operation: "harvest_batch", NextAction: resources.DiskActionRecoverSpace},
		}}
	}
	if a.retargetFrom != "" {
		_ = os.Remove(a.retargetFrom)
		_ = os.Symlink(a.retargetTo, a.retargetFrom)
	}
	return nil
}

type capacityBehaviorVerifier struct{ calls int }

func (v *capacityBehaviorVerifier) Execute(context.Context, string) (*VerifyResult, error) {
	v.calls++
	return &VerifyResult{Passed: true}, nil
}

type capacityBehaviorDispatcher struct{ calls int }

func (d *capacityBehaviorDispatcher) BoardComplete(context.Context, string, string) error {
	d.calls++
	return nil
}

type capacityBehaviorSession struct{ calls int }

func (s *capacityBehaviorSession) Stop(context.Context, string) error {
	s.calls++
	return nil
}

type identityDriftBackend struct {
	mu    sync.Mutex
	calls int
}

func (b *identityDriftBackend) StatFS(string) (resources.Capacity, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	id := "planned-volume"
	if b.calls > 3 {
		id = "fresh-volume"
	}
	return resources.Capacity{FilesystemID: id, TotalBytes: 1 << 40, FreeBytes: 1 << 40, TotalInodes: 1 << 30, FreeInodes: 1 << 30}, nil
}

func newCapacityBehaviorFixture(t *testing.T, worktreeCount int) (*Harvester, *capacityBehaviorCommands) {
	t.Helper()
	root := t.TempDir()
	commands := &capacityBehaviorCommands{root: root, order: &capacityBehaviorTimeline{}, branchOutputs: []string{"main"}}
	for i := 0; i < worktreeCount; i++ {
		wt := filepath.Join(root, "wt-"+string(rune('a'+i)))
		if err := os.MkdirAll(wt, 0o755); err != nil {
			t.Fatal(err)
		}
		commands.wts = append(commands.wts, wt)
	}
	h := NewHarvester(root)
	return h, commands
}

func TestHarvestAndIntegrationCapacityDenialsStopRealCallbacks(t *testing.T) {
	for _, stage := range []string{"plan", "admit"} {
		t.Run(stage, func(t *testing.T) {
			h, commands := newCapacityBehaviorFixture(t, 1)
			admission := &capacityBehaviorAdmission{stage: stage}
			admission.order = commands.order
			h.DiskAdmission = admission
			old := execCommandContext
			defer func() { execCommandContext = old }()
			execCommandContext = commands.command

			verifier := &capacityBehaviorVerifier{}
			dispatcher := &capacityBehaviorDispatcher{}
			session := &capacityBehaviorSession{}
			in := NewIntegration(h, verifier, dispatcher, nil, commands.root, WithSessionManager(session))
			_, err := in.Run(context.Background())
			if err == nil {
				t.Fatal("expected top-level harvest denial")
			}
			if commands.count("fetch") != 0 || commands.count("push") != 0 || commands.count("worktree remove") != 0 {
				t.Fatalf("mutation commands after %s denial: %+v", stage, commands.calls)
			}
			if verifier.calls != 0 || dispatcher.calls != 0 || session.calls != 0 {
				t.Fatalf("callbacks after %s denial: verifier=%d dispatcher=%d cleanup=%d", stage, verifier.calls, dispatcher.calls, session.calls)
			}
			wantAction := `next_action":"retry_capacity_probe"`
			if stage == "admit" {
				wantAction = `next_action":"recover_capacity_without_cleanup"`
			}
			if !strings.Contains(err.Error(), wantAction) {
				t.Fatalf("denial did not preserve concrete NextAction %q: %v", wantAction, err)
			}
		})
	}
}

func TestPublicDirectFetchWrappersAdmitBeforeRealFetch(t *testing.T) {
	for _, wrapper := range []struct {
		name string
		call func(*Harvester, context.Context, string) error
	}{
		{name: "normal", call: func(h *Harvester, ctx context.Context, wt string) error { _, err := h.UnmergedFor(ctx, wt); return err }},
		{name: "strict", call: func(h *Harvester, ctx context.Context, wt string) error {
			_, err := h.UnmergedForStrict(ctx, wt)
			return err
		}},
	} {
		t.Run(wrapper.name, func(t *testing.T) {
			h, commands := newCapacityBehaviorFixture(t, 1)
			admission := &capacityBehaviorAdmission{stage: "admit", order: commands.order}
			commands.branchOutputs = []string{"feature"}
			h.DiskAdmission = admission
			old := execCommandContext
			defer func() { execCommandContext = old }()
			execCommandContext = commands.command
			if err := wrapper.call(h, context.Background(), commands.wts[0]); err == nil {
				t.Fatal("expected direct admission denial")
			}
			if commands.count("fetch") != 0 {
				t.Fatalf("direct wrapper fetched before admission: %+v", commands.calls)
			}
		})
	}
}

func TestSameRootFeatureDirectFetchUsesExactAdmissionToken(t *testing.T) {
	h, commands := newCapacityBehaviorFixture(t, 0)
	admission := &capacityBehaviorAdmission{order: commands.order}
	commands.branchOutputs = []string{"feature"}
	h.DiskAdmission = admission
	old := execCommandContext
	defer func() { execCommandContext = old }()
	execCommandContext = commands.command
	if _, err := h.UnmergedFor(context.Background(), commands.root); err != nil {
		t.Fatal(err)
	}
	if admission.plans != 1 || admission.batchPlans != 1 || commands.count("fetch") != 1 || commands.order.index("plan") > commands.order.index("git:fetch") || commands.order.index("admit") > commands.order.index("git:fetch") {
		t.Fatalf("same-root direct bypassed exact admission: plans=%d admissions=%d commands=%v", admission.plans, admission.batchPlans, commands.calls)
	}
}

func TestDirectRetargetUsesAdmittedCanonicalDirectoryForAllGitCommands(t *testing.T) {
	root := t.TempDir()
	realWorktree := filepath.Join(root, "real-worktree")
	alias := filepath.Join(root, "alias-worktree")
	newTarget := filepath.Join(root, "retargeted-worktree")
	for _, path := range []string{realWorktree, newTarget} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realWorktree, alias); err != nil {
		t.Fatal(err)
	}
	dirLog := filepath.Join(root, "dirs.log")
	commands := &capacityBehaviorCommands{root: root, order: &capacityBehaviorTimeline{}, branchOutputs: []string{"feature"}, dirLog: dirLog, retargetFrom: alias, retargetTo: newTarget}
	admission := &capacityBehaviorAdmission{order: commands.order}
	h := NewHarvester(root)
	h.DiskAdmission = admission
	old := execCommandContext
	defer func() { execCommandContext = old }()
	execCommandContext = commands.command
	if _, err := h.UnmergedFor(context.Background(), alias); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dirLog)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realWorktree)
	if err != nil {
		t.Fatal(err)
	}
	for _, observed := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if observed != want {
			t.Fatalf("git command used %q, want admitted canonical %q", observed, want)
		}
	}
	if admission.batchPlans != 1 || commands.count("fetch") != 1 || commands.count("cherry") != 1 {
		t.Fatalf("direct command authority drifted: admissions=%d commands=%v", admission.batchPlans, commands.calls)
	}
}

func TestSuccessfulBatchAdmissionOrderAndExactlyOnceScopes(t *testing.T) {
	h, commands := newCapacityBehaviorFixture(t, 2)
	admission := &capacityBehaviorAdmission{}
	admission.order = commands.order
	h.DiskAdmission = admission
	old := execCommandContext
	defer func() { execCommandContext = old }()
	execCommandContext = commands.command
	if _, err := h.Harvest(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstFetch := commands.index("fetch")
	if firstFetch < 0 || commands.index("worktree list") > firstFetch || commands.order.index("plan") > commands.order.index("git:fetch") || commands.order.index("admit") > commands.order.index("git:fetch") {
		t.Fatalf("missing fetch/list ordering: %+v", commands.calls)
	}
	admission.mu.Lock()
	events := append([]string(nil), admission.events...)
	scopes := append([]string(nil), admission.admittedScopes...)
	plans, batchPlans := admission.plans, admission.batchPlans
	admission.mu.Unlock()
	firstTimelineFetch := commands.order.index("git:fetch")
	if plans != 1 || batchPlans != 1 || len(scopes) != 2 {
		t.Fatalf("plans=%d batch=%d admitted scopes=%v", plans, batchPlans, scopes)
	}
	for _, scope := range scopes {
		if commands.order.index("admit:"+scope) >= firstTimelineFetch {
			t.Fatalf("scope %s admitted after fetch: %v", scope, commands.order.entries)
		}
	}
	if len(events) != 4 || events[0] != "plan" || events[1] != "admit" || !strings.HasPrefix(events[2], "admit:") || !strings.HasPrefix(events[3], "admit:") {
		t.Fatalf("admission order/events = %v", events)
	}
	if commands.count("fetch") != 2 {
		t.Fatalf("fetch count = %d, want two", commands.count("fetch"))
	}
}

func TestCanonicalSymlinkMappingCannotTriggerSecondAdmission(t *testing.T) {
	root := t.TempDir()
	realWorktree := filepath.Join(root, "real-worktree")
	alias := filepath.Join(root, "alias-worktree")
	if err := os.MkdirAll(realWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realWorktree, alias); err != nil {
		t.Fatal(err)
	}
	commands := &capacityBehaviorCommands{root: root, wts: []string{alias}, order: &capacityBehaviorTimeline{}}
	admission := &capacityBehaviorAdmission{order: commands.order}
	h := NewHarvester(root)
	h.DiskAdmission = admission
	old := execCommandContext
	defer func() { execCommandContext = old }()
	execCommandContext = commands.command
	if _, err := h.Harvest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if admission.batchPlans != 1 || commands.count("fetch") != 1 {
		t.Fatalf("canonical alias caused partial/second path: admissions=%d commands=%v", admission.batchPlans, commands.calls)
	}
}

func TestBatchFetchRetargetedSymlinkFailsBeforeFetch(t *testing.T) {
	h, commands := newCapacityBehaviorFixture(t, 1)
	oldTarget := commands.wts[0]
	newTarget := filepath.Join(commands.root, "retargeted-worktree")
	if err := os.MkdirAll(newTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(commands.root, "retarget-alias")
	if err := os.Symlink(oldTarget, alias); err != nil {
		t.Fatal(err)
	}
	commands.wts[0] = alias
	admission := &capacityBehaviorAdmission{order: commands.order, retargetFrom: alias, retargetTo: newTarget}
	h.DiskAdmission = admission
	old := execCommandContext
	defer func() { execCommandContext = old }()
	execCommandContext = commands.command
	if _, err := h.Harvest(context.Background()); err == nil {
		t.Fatal("expected retarget denial")
	}
	if commands.count("fetch") != 0 || admission.batchPlans != 1 {
		t.Fatalf("retargeted path fetched or re-admitted: fetches=%d admissions=%d commands=%v", commands.count("fetch"), admission.batchPlans, commands.calls)
	}
}

func TestFreshIdentityDriftStopsIntegrationBeforeDownstreamCallbacks(t *testing.T) {
	h, commands := newCapacityBehaviorFixture(t, 1)
	h.DiskAdmission = resources.NewCapacityGate(&identityDriftBackend{}, resources.DiskPolicy{ReserveBytes: 1, ReserveInodes: 1})
	verifier := &capacityBehaviorVerifier{}
	dispatcher := &capacityBehaviorDispatcher{}
	session := &capacityBehaviorSession{}
	in := NewIntegration(h, verifier, dispatcher, nil, commands.root, WithSessionManager(session))
	old := execCommandContext
	defer func() { execCommandContext = old }()
	execCommandContext = commands.command
	if _, err := in.Run(context.Background()); err == nil {
		t.Fatal("expected fresh identity drift denial")
	}
	if commands.count("fetch") != 0 || commands.count("push") != 0 || commands.count("worktree remove") != 0 || verifier.calls != 0 || dispatcher.calls != 0 || session.calls != 0 {
		t.Fatalf("downstream callback after identity drift: commands=%v verifier=%d dispatcher=%d cleanup=%d", commands.calls, verifier.calls, dispatcher.calls, session.calls)
	}
}

func TestTwoTargetRetargetPreflightLaunchesNoGoroutines(t *testing.T) {
	h, commands := newCapacityBehaviorFixture(t, 2)
	firstReal, secondReal := commands.wts[0], commands.wts[1]
	firstAlias := filepath.Join(commands.root, "first-alias")
	secondAlias := filepath.Join(commands.root, "second-alias")
	if err := os.Symlink(firstReal, firstAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secondReal, secondAlias); err != nil {
		t.Fatal(err)
	}
	newTarget := filepath.Join(commands.root, "retargeted")
	if err := os.MkdirAll(newTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	commands.wts = []string{firstAlias, secondAlias}
	admission := &capacityBehaviorAdmission{order: commands.order, retargetFrom: firstAlias, retargetTo: newTarget}
	h.DiskAdmission = admission
	old := execCommandContext
	defer func() { execCommandContext = old }()
	execCommandContext = commands.command
	if _, err := h.Harvest(context.Background()); err == nil {
		t.Fatal("expected complete-set retarget denial")
	}
	if commands.count("rev-parse") != 1 || commands.count("fetch") != 0 {
		t.Fatalf("retarget discovered after fan-out: rev-parse=%d fetch=%d commands=%v", commands.count("rev-parse"), commands.count("fetch"), commands.calls)
	}
}

func TestFeatureRootIsAdmittedAndInventoried(t *testing.T) {
	h, commands := newCapacityBehaviorFixture(t, 1)
	commands.branchOutputs = []string{"feature", "feature"}
	admission := &capacityBehaviorAdmission{order: commands.order}
	h.DiskAdmission = admission
	old := execCommandContext
	defer func() { execCommandContext = old }()
	execCommandContext = commands.command
	if _, err := h.Harvest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if admission.plans != 1 || admission.batchPlans != 1 || commands.count("fetch") != 2 {
		t.Fatalf("feature root bypassed admission/inventory: plans=%d admissions=%d fetches=%d", admission.plans, admission.batchPlans, commands.count("fetch"))
	}
}

// Independent RED mutants covered by these behavioral fixtures:
// FAC153-M11 validate tokens inside the goroutine loop instead of before wg.Add.
// FAC153-M12 replace byOriginal/byCanonical maps with repeated item scans.
// FAC153-M13 restore sequential DiskAdmission fallback for batch plans.
// FAC153-M14 launch fetch before the shared plan/admission timeline completes.
// FAC153-M15 swallow a batch denial into result.Errors and continue Integration.
// FAC153-M16 map tokens by original rather than canonical symlink identity.
// FAC153-M17 remove fresh-to-planned filesystem identity validation from
// CapacityGate.AdmitDiskPlan; the identity-drift fixture must turn RED.
// FAC153-M18 remove post-admission canonical re-resolution before batch fetch.
// FAC153-M19 restore same-root direct len(items)!=1 silent success.
// FAC153-M20 classify direct branch before admission or run cherry in the
// original path; the directory-identity fixture must turn RED.
