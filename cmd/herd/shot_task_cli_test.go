package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/shot"
)

// FAC-89 compiled-CLI contract for the bounded ONE-TASK lane. Every case here
// drives the real binary in a throwaway git repository: no board, no fleet, no
// network, no state outside t.TempDir().

const cliCandidate = "3333333333333333333333333333333333333333"

func shotRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "shot-cli@test.invalid"},
		{"config", "user.name", "Shot CLI Test"},
		{"config", "commit.gpgSign", "false"},
		{"commit", "--allow-empty", "-q", "-m", "base"},
	} {
		if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func runShotCLI(t *testing.T, binary, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	// XDG_STATE_HOME keeps any ledger write inside the test's own tree.
	// stubHarnessPATH makes routing deterministic: this test is about which LANE
	// an argument takes, not about what happens to be installed on the host.
	cmd.Env = append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"PATH="+stubHarnessPATH(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("herd shot %v: %v\n%s", args, err, out)
	}
	return string(out), exitErr.ExitCode()
}

func TestShotHelpNeverEntersOperationalCode(t *testing.T) {
	binary := buildHerd(t)
	probe := filepath.Join(t.TempDir(), "probe")
	for _, args := range [][]string{{"shot", "--help"}, {"shot", "-h"}} {
		c := exec.Command(binary, args...)
		c.Env = append(os.Environ(), helpProbeEnv+"="+probe)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if !strings.Contains(string(out), "<task-ref>") {
			t.Fatalf("shot help must document the task lane:\n%s", out)
		}
		if data, readErr := os.ReadFile(probe); readErr == nil && strings.Contains(string(data), "shot") {
			t.Fatalf("help path entered operational code: %q", string(data))
		}
	}
}

// A task ref routes to the bounded lane; anything else stays on the prompt
// lane. --dry-run proves the routing without executing a provider: it exists
// only on the prompt lane, and it stops before any launch.
func TestShotRoutesByFirstArgument(t *testing.T) {
	binary := buildHerd(t)
	repo := shotRepo(t)

	// --dry-run must precede the prompt: Go's flag parser stops at the first
	// positional, so a trailing flag would be delivered as prompt text.
	out, code := runShotCLI(t, binary, repo, "shot", "--dry-run", "not-a-ref-prompt")
	if code != 0 || !strings.Contains(out, "would run") {
		t.Fatalf("prompt text must stay on the prompt lane (%d):\n%s", code, out)
	}

	// The same flag on the task lane is unknown — proof the ref switched lanes.
	out, code = runShotCLI(t, binary, repo, "shot", "FAC-89", "--dry-run")
	if code == 0 || strings.Contains(out, "would run") {
		t.Fatalf("a task ref must not enter the prompt lane (%d):\n%s", code, out)
	}
}

func TestShotPromptLaneStillRequiresAPrompt(t *testing.T) {
	binary := buildHerd(t)
	out, code := runShotCLI(t, binary, shotRepo(t), "shot")
	if code == 0 {
		t.Fatalf("bare `herd shot` must fail:\n%s", out)
	}
	if !strings.Contains(out, "prompt required") {
		t.Fatalf("bare `herd shot` must still name the prompt lane's contract:\n%s", out)
	}
}

// The completion callback is the builder half of the loop. This drives it
// through the real binary and reads the durable mailbox back.
func TestShotReportPostsDurableCallback(t *testing.T) {
	binary := buildHerd(t)
	repo := shotRepo(t)

	out, code := runShotCLI(t, binary, repo, "shot", "FAC-89",
		"--report", "complete", "--sha", cliCandidate, "--lease", "4")
	if code != 0 {
		t.Fatalf("posting a completion callback failed (%d):\n%s", code, out)
	}

	mb := mail.NewMailbox(filepath.Join(repo, ".herd", "control-mail.jsonl"))
	callbacks, err := mb.DrainCallbacks()
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(callbacks) != 1 {
		t.Fatalf("want exactly one callback, got %d: %+v", len(callbacks), callbacks)
	}
	cb := callbacks[0]
	if cb.Ref != "FAC-89" || cb.Kind != mail.CallbackComplete {
		t.Fatalf("callback lost its identity: %+v", cb)
	}
	if cb.SHA != cliCandidate || cb.LeaseGeneration != 4 {
		t.Fatalf("callback lost the candidate or the lease it is fenced by: %+v", cb)
	}
}

func TestShotReportBlockedCarriesDetail(t *testing.T) {
	binary := buildHerd(t)
	repo := shotRepo(t)

	if out, code := runShotCLI(t, binary, repo, "shot", "FAC-89",
		"--report", "blocked", "--detail", "waiting on FAC-172", "--lease", "4"); code != 0 {
		t.Fatalf("posting a blocked callback failed (%d):\n%s", code, out)
	}
	callbacks, err := mail.NewMailbox(filepath.Join(repo, ".herd", "control-mail.jsonl")).DrainCallbacks()
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(callbacks) != 1 || callbacks[0].Kind != mail.CallbackBlocked {
		t.Fatalf("want one blocked callback, got %+v", callbacks)
	}
	if callbacks[0].Detail != "waiting on FAC-172" {
		t.Fatalf("blocked callback lost its detail: %+v", callbacks[0])
	}
}

// An unfenced or unanchored report is refused, and nothing reaches the mailbox.
func TestShotReportRefusesUnprovableReports(t *testing.T) {
	binary := buildHerd(t)
	for name, args := range map[string][]string{
		"no lease":       {"shot", "FAC-89", "--report", "complete", "--sha", cliCandidate},
		"no sha":         {"shot", "FAC-89", "--report", "complete", "--lease", "4"},
		"blocked no why": {"shot", "FAC-89", "--report", "blocked", "--lease", "4"},
		"unknown kind":   {"shot", "FAC-89", "--report", "finished", "--lease", "4"},
	} {
		repo := shotRepo(t)
		out, code := runShotCLI(t, binary, repo, args...)
		if code == 0 {
			t.Fatalf("%s: must be refused:\n%s", name, out)
		}
		if _, err := os.Stat(filepath.Join(repo, ".herd", "control-mail.jsonl")); err == nil {
			t.Fatalf("%s: a refused report still wrote to the mailbox", name)
		}
	}
}

// The evidence packet is the whole point of --json: a failing shot must still
// emit parseable, staged evidence on stdout and exit non-zero.
func TestShotEmitsJSONEvidenceOnFailure(t *testing.T) {
	binary := buildHerd(t)
	repo := shotRepo(t)

	// No .herd/herd.yaml: the lock is taken, then the eligibility stage fails.
	out, code := runShotCLI(t, binary, repo, "shot", "FAC-89", "--json", "--timeout", "5")
	if code == 0 {
		t.Fatalf("a shot with no board configuration must exit non-zero:\n%s", out)
	}
	ev := decodeEvidence(t, out)
	if ev.TaskRef != "FAC-89" || ev.OK {
		t.Fatalf("evidence must name the failed task: %+v", ev)
	}
	if ev.Stage != shot.StageEligibility {
		t.Fatalf("want stage %s (the lock is taken before the board is read), got %s", shot.StageEligibility, ev.Stage)
	}
	if ev.Recoverable {
		t.Fatal("nothing was claimed; this retry is clean, not recoverable state")
	}
	if ev.Error == "" {
		t.Fatalf("evidence must carry the reason: %+v", ev)
	}
	// A refused shot leaves no claim behind — including no held lock.
	if _, err := os.Stat(filepath.Join(repo, ".herd", "locks", "shot-fac-89.lock.d")); err == nil {
		t.Fatal("the shot lock outlived the process")
	}
}

// Duplicate invocations must not double-claim: the second refuses at the lock
// stage without ever reading the board.
func TestShotRefusesDuplicateInvocation(t *testing.T) {
	binary := buildHerd(t)
	repo := shotRepo(t)

	lockDir := filepath.Join(repo, ".herd", "locks", "shot-fac-89.lock.d")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// This test process is a live holder, so the guard must not break the lock.
	holder := []byte(strconv.Itoa(os.Getpid()) + "\n")
	if err := os.WriteFile(filepath.Join(lockDir, "holder"), holder, 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runShotCLI(t, binary, repo, "shot", "FAC-89", "--json", "--timeout", "5")
	if code == 0 {
		t.Fatalf("a second shot for the same task must be refused:\n%s", out)
	}
	ev := decodeEvidence(t, out)
	if ev.Stage != shot.StageLock {
		t.Fatalf("want a refusal at %s, got %s (%+v)", shot.StageLock, ev.Stage, ev)
	}
	if ev.Recoverable {
		t.Fatal("the duplicate claimed nothing; its refusal is clean")
	}
	// The first holder's lock is still theirs.
	if _, err := os.Stat(lockDir); err != nil {
		t.Fatalf("the duplicate removed the live holder's lock: %v", err)
	}
}

func TestShotRejectsNonPositiveTimeout(t *testing.T) {
	binary := buildHerd(t)
	out, code := runShotCLI(t, binary, shotRepo(t), "shot", "FAC-89", "--timeout", "0")
	if code != 2 {
		t.Fatalf("want usage exit 2, got %d:\n%s", code, out)
	}
}

// decodeEvidence pulls the evidence packet out of the combined stream. Progress
// prose goes to stderr in --json mode, so the JSON object must still parse.
func decodeEvidence(t *testing.T, out string) shot.Evidence {
	t.Helper()
	start := strings.Index(out, "{")
	end := strings.LastIndex(out, "}")
	if start < 0 || end < start {
		t.Fatalf("no evidence packet in output:\n%s", out)
	}
	var ev shot.Evidence
	if err := json.Unmarshal([]byte(out[start:end+1]), &ev); err != nil {
		t.Fatalf("evidence is not valid JSON (%v):\n%s", err, out)
	}
	return ev
}

// stubHarnessPATH returns a directory holding no-op harness executables.
//
// The router picks a surface by looking the CLI up on PATH. On a developer box
// codex/claude/grok are installed so a bounded shot always routes; in CI none
// are, and this test failed with `no eligible surface left for a "bounded" shot
// (tried 0)` — it was asserting the host's tooling, not the argument routing it
// claims to cover. The stubs are never executed: --dry-run stops before launch.
func stubHarnessPATH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"claude", "grok"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	// codex, pi, and opencode get PROBE-FAITHFUL stubs, not no-ops. The write-capable
	// tool probe (pkg/toolprobe/probe.go recipeCommand) shells `pi` or `codex` for
	// harness="codex"/harness="pi"/provider="codex" and `opencode` for harness="opencode".
	// Both judge the ARTIFACT, never the model's claim: they require the file
	// named in the prompt to exist containing EXECUTED. A stub that merely
	// exits 0 is correctly judged INCAPABLE — "model described the write but
	// did not execute the tool" — which is exactly what that gate is for.
	// So the stub performs the write the probe demands.
	probe := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  for w in $a; do\n" +
		"    case \"$w\" in */PROBE_OK.txt)\n" +
		"      mkdir -p \"$(dirname \"$w\")\" && printf 'EXECUTED' > \"$w\" ;;\n" +
		"    esac\n" +
		"  done\n" +
		"done\n" +
		"printf 'PROBE_OK\\n'\n" +
		"exit 0\n"
	for _, name := range []string{"codex", "pi", "opencode"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(probe), 0o755); err != nil {
			t.Fatalf("write %s probe stub: %v", name, err)
		}
	}
	return dir
}
