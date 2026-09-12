package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/process"
)

// FAC-36 subprocess fixtures.
//
// The in-process tests in process_digest_test.go replace the read seams, so
// they prove the ADAPTER's logic and nothing about the transport. These drive
// the compiled `herd process` binary against a fake herdr executable, which is
// the only way to exercise what the review actually asked about: the process
// boundary, the byte bounds, and cancellation of a real child.
//
// Every fixture sets the hermeticity guard, so a resolution bug cannot reach
// the operator's live fleet — it fails instead.

const fakeProcessWorkspace = "wT"

// processFakeRoster is the healthy roster: two in-scope agents and one agent
// belonging to a different workspace, which must never be read.
const processFakeRoster = `{"id":1,"result":{"agents":[` +
	`{"name":"builder-a","pane_id":"wT:p1","workspace_id":"wT","agent_status":"working"},` +
	`{"name":"reviewer-b","pane_id":"wT:p2","workspace_id":"wT","agent_status":"idle"},` +
	`{"name":"stranger","pane_id":"wX:p9","workspace_id":"wX","agent_status":"idle"}` +
	`]}}`

// installProcessFake writes a fake herdr whose pane-read behaviour is chosen
// by mode, and returns the env for the CLI plus the call-log path.
//
// LookPath guards: the fake is an absolute path handed to herdr.BinaryEnv, and
// herdr.NoLiveEnv is set so binaryPath REFUSES if the override is ever lost.
// Both are asserted before the fixture returns, and the directory holding the
// fake is deliberately NOT put on PATH, so a bare "herdr" lookup cannot find
// it either: the only way a read happens is through the override.
func installProcessFake(t *testing.T, roster, mode string) (env []string, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	logPath = filepath.Join(dir, "calls.log")
	rosterPath := filepath.Join(dir, "roster.json")
	modePath := filepath.Join(dir, "mode")

	for path, content := range map[string]string{
		logPath:    "",
		rosterPath: roster,
		modePath:   mode,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Response shapes follow the installed herdr contract (0.9.0,
	// `herdr api schema --json`): success is {id, result}, error is
	// {id, error:{code,message}}.
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$HERD_FAKE_LOG"
case "$1 $2" in
  "agent list")
    cat "$HERD_FAKE_ROSTER"
    ;;
  "pane read")
    case "$(cat "$HERD_FAKE_MODE")" in
      hang)     sleep 600 ;;
      flood)
        # Build one 8KB block and repeat it, so the 1MB bound is reached in
        # ~128 writes instead of tens of thousands of shell iterations. A
        # fixture that proves a bound must not itself burn the host's CPU.
        b=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
        b=$b$b$b$b ; b=$b$b$b$b ; b=$b$b$b$b ; b=$b$b$b$b
        while : ; do printf '%s' "$b" ; done
        ;;
      # A 0.9.0 error envelope.
      envelope) printf '{"id":1,"error":{"code":404,"message":"pane %s is gone"}}\n' "$3" ;;
      # A bare top-level error object: no id, no result. Must fail closed.
      bare-error) printf '{"error":"pane %s vanished"}\n' "$3" ;;
      # A structured reply that stops mid-object. Must fail closed, never be
      # demoted to "probably raw text".
      malformed) printf '{"id":1,"result":{"text":"half\n' ;;
      # A well-formed envelope whose result body is unknown. An unrecognised
      # body is an unread pane, not an empty one.
      unsupported) printf '{"id":1,"result":{"payload":1}}\n' ;;
      # The agent's OWN output is an error object, delivered inside a valid
      # result.text. This is ordinary pane content and must stay that way.
      literal)  printf '{"id":1,"result":{"text":"{\\"error\\":\\"the agent printed this object itself\\"}","truncated":false}}\n' ;;
      # herdr states that it cut the tail itself.
      cut)      printf '{"id":1,"result":{"text":"PASS: 12 tests, 0 failures","truncated":true}}\n' ;;
      # No envelope at all: legacy raw text.
      rawtext)  printf 'PASS: 12 tests, 0 failures\n' ;;
      empty)    printf '{"id":1,"result":{"text":"","truncated":false}}\n' ;;
      *)        printf '{"id":1,"result":{"text":"PASS: 12 tests, 0 failures\\n","truncated":false}}\n' ;;
    esac
    ;;
  *)
    printf '{"id":1,"result":{}}\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Guard: the fake must be resolvable through the override, and the live
	// fleet must be unreachable without it.
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		t.Fatalf("fake herdr is not an executable file: %v", err)
	}
	if resolved, err := exec.LookPath(bin); err != nil || resolved != bin {
		t.Fatalf("LookPath(%s) = (%q, %v)", bin, resolved, err)
	}
	if found, err := exec.LookPath("herdr"); err == nil && found == bin {
		t.Fatalf("the fake leaked onto PATH at %s; the override must be the only route", found)
	}

	env = []string{
		herdr.BinaryEnv + "=" + bin,
		herdr.NoLiveEnv + "=1",
		"HERD_FAKE_LOG=" + logPath,
		"HERD_FAKE_ROSTER=" + rosterPath,
		"HERD_FAKE_MODE=" + modePath,
	}
	return env, logPath
}

// processCalls returns the fake's call log, one herdr invocation per line.
func processCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func countCalls(calls []string, prefix string) int {
	n := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

func decodeProcessJSON(t *testing.T, out []byte) processDigestEnvelope {
	t.Helper()
	line := ""
	for _, candidate := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(candidate), "{") {
			line = strings.TrimSpace(candidate)
		}
	}
	if line == "" {
		t.Fatalf("no JSON object in output:\n%s", out)
	}
	var envelope processDigestEnvelope
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatalf("decode %s: %v", line, err)
	}
	return envelope
}

// A healthy populated fleet: both in-scope panes are read exactly once, the
// out-of-scope agent is not read at all, and the digest carries real identities
// rather than the pane-demo placeholder this command used to print.
func TestProcessCLIHealthyPopulatedFleet(t *testing.T) {
	dir := t.TempDir()
	env, logPath := installProcessFake(t, processFakeRoster, "healthy")
	out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace)
	if err != nil {
		t.Fatalf("herd process: %v\n%s", err, out)
	}
	envelope := decodeProcessJSON(t, out)
	if envelope.Partial {
		t.Fatalf("a healthy sweep reported partial: %s", out)
	}
	if envelope.PaneReads != 2 {
		t.Fatalf("pane_reads = %d, want 2", envelope.PaneReads)
	}
	if len(envelope.Items) != 2 {
		t.Fatalf("items = %d, want 2:\n%s", len(envelope.Items), out)
	}
	names := map[string]string{}
	for _, item := range envelope.Items {
		names[item.Name] = item.PaneID
	}
	if names["builder-a"] != "wT:p1" || names["reviewer-b"] != "wT:p2" {
		t.Fatalf("digest lost the real pane identities: %+v", names)
	}
	if _, leaked := names["stranger"]; leaked {
		t.Fatalf("an out-of-scope agent was included: %+v", names)
	}

	// Exact request counts: one roster read and one pane read per in-scope
	// agent. Nothing mutating was invoked.
	calls := processCalls(t, logPath)
	if got := countCalls(calls, "agent list"); got != 1 {
		t.Fatalf("roster read %d times, want exactly 1:\n%v", got, calls)
	}
	if got := countCalls(calls, "pane read"); got != 2 {
		t.Fatalf("pane read %d times, want exactly 2:\n%v", got, calls)
	}
	if len(calls) != 3 {
		t.Fatalf("herdr called %d times, want exactly 3:\n%v", len(calls), calls)
	}
	for _, forbidden := range []string{"agent prompt", "send-keys", "kill", "signal", "agent stop", "workspace"} {
		for _, call := range calls {
			if strings.Contains(call, forbidden) {
				t.Fatalf("a read-only digest invoked %q: %v", forbidden, calls)
			}
		}
	}
	if strings.Contains(strings.Join(calls, "\n"), "wX:p9") {
		t.Fatalf("the out-of-scope pane was read: %v", calls)
	}
}

// An empty in-scope fleet is a SUCCESS with zero items, and no pane is read.
// This is the case that must stay distinguishable from a broken roster.
func TestProcessCLIEmptyFleetSucceedsWithoutReadingAnyPane(t *testing.T) {
	dir := t.TempDir()
	env, logPath := installProcessFake(t, `{"result":{"agents":[]}}`, "healthy")
	out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace)
	if err != nil {
		t.Fatalf("an empty fleet must succeed: %v\n%s", err, out)
	}
	envelope := decodeProcessJSON(t, out)
	if envelope.Partial || envelope.PaneReads != 0 || len(envelope.Items) != 0 {
		t.Fatalf("empty fleet = partial:%v reads:%d items:%d", envelope.Partial, envelope.PaneReads, len(envelope.Items))
	}
	calls := processCalls(t, logPath)
	if len(calls) != 1 || countCalls(calls, "agent list") != 1 {
		t.Fatalf("empty fleet issued %v", calls)
	}
}

// A roster that is not a transport envelope, or that is an ERROR envelope, is
// a hard failure. A broken roster must never present as an empty fleet.
func TestProcessCLIMalformedRosterFailsInsteadOfReportingAnEmptyFleet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		roster string
	}{
		{"truncated json", `{"id":1,"result":{"agents":[`},
		{"bare error object", `{"error":"herdr is not running"}`},
		{"0.9.0 error envelope", `{"id":1,"error":{"code":500,"message":"daemon down"}}`},
		{"id only", `{"id":1}`},
		{"error plus result", `{"id":1,"error":"partial roster","result":{"agents":[{"name":"a","pane_id":"wT:p1","workspace_id":"wT"}]}}`},
		{"raw text", "herdr: command not found"},
		{"null agents", `{"id":1,"result":{"agents":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			env, logPath := installProcessFake(t, tc.roster, "healthy")
			out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace)
			if err == nil {
				t.Fatalf("a broken roster exited 0:\n%s", out)
			}
			if strings.Contains(string(out), `"items":[]`) {
				t.Fatalf("a broken roster printed an empty fleet digest:\n%s", out)
			}
			calls := processCalls(t, logPath)
			if countCalls(calls, "pane read") != 0 {
				t.Fatalf("panes were read despite an unreadable roster: %v", calls)
			}
		})
	}
}

// An error envelope from a PANE is a transport failure: the sweep continues,
// reports the pane as UNKNOWN, marks itself partial, and exits nonzero. The
// envelope text must never be classified as the agent's own verdict.
func TestProcessCLIPaneErrorEnvelopeIsPartialNotAVerdict(t *testing.T) {
	dir := t.TempDir()
	env, logPath := installProcessFake(t, processFakeRoster, "envelope")
	out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace)
	if err == nil {
		t.Fatalf("a sweep that read no pane successfully exited 0:\n%s", out)
	}
	envelope := decodeProcessJSON(t, out)
	if !envelope.Partial {
		t.Fatalf("pane transport failures were not reported as partial:\n%s", out)
	}
	if len(envelope.Unknowns) != 2 {
		t.Fatalf("unknowns = %v, want one per failed pane", envelope.Unknowns)
	}
	got := strings.Join(envelope.Unknowns, "; ")
	if !strings.Contains(got, "transport error envelope") {
		t.Fatalf("failure reason = %q", got)
	}
	// herdr's own message is the operator's only clue about why the read
	// failed, so it must survive into the machine-readable envelope.
	if !strings.Contains(got, "is gone") {
		t.Fatalf("the transport message was dropped from the digest: %q", got)
	}
	for _, item := range envelope.Items {
		if item.Class != process.Unknown {
			t.Fatalf("%s classified %s from an error envelope, want UNKNOWN", item.Name, item.Class)
		}
		if strings.Contains(item.Tail, "is gone") {
			t.Fatalf("envelope text reached classification as agent output: %+v", item)
		}
	}
	if got := countCalls(processCalls(t, logPath), "pane read"); got != 2 {
		t.Fatalf("pane read %d times, want 2", got)
	}
}

// A pane whose OWN output is a literal JSON error object is agent content. It
// is classified normally, and the sweep stays complete.
func TestProcessCLILiteralAgentJSONIsContentNotATransportFailure(t *testing.T) {
	dir := t.TempDir()
	env, _ := installProcessFake(t, processFakeRoster, "literal")
	out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace)
	if err != nil {
		t.Fatalf("literal agent JSON failed the sweep: %v\n%s", err, out)
	}
	envelope := decodeProcessJSON(t, out)
	if envelope.Partial {
		t.Fatalf("literal agent JSON was misread as a transport failure: %v", envelope.Unknowns)
	}
	if envelope.PaneReads != 2 {
		t.Fatalf("pane_reads = %d, want 2", envelope.PaneReads)
	}
	for _, item := range envelope.Items {
		if !strings.Contains(item.Tail, "the agent printed this object itself") {
			t.Fatalf("agent content did not reach classification: %+v", item)
		}
	}
}

// An empty pane is readable and empty — a success, not a failure.
func TestProcessCLIEmptyPaneTextIsASuccessfulRead(t *testing.T) {
	dir := t.TempDir()
	env, _ := installProcessFake(t, processFakeRoster, "empty")
	out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace)
	if err != nil {
		t.Fatalf("an empty pane failed the sweep: %v\n%s", err, out)
	}
	envelope := decodeProcessJSON(t, out)
	if envelope.Partial || envelope.PaneReads != 2 {
		t.Fatalf("empty panes = partial:%v reads:%d", envelope.Partial, envelope.PaneReads)
	}
}

// A child that hangs forever is stopped by the SWEEP deadline, not by a
// per-read timer: with two hung panes and a 3s bound, the command must return
// in about 3s, not 6s. The roster read shares that same bound.
func TestProcessCLIHungPanesStopAtTheSweepDeadline(t *testing.T) {
	const deadline = 3 * time.Second
	dir := t.TempDir()
	env, logPath := installProcessFake(t, processFakeRoster, "hang")
	buildHerd(t) // pay the build cost before the clock starts
	start := time.Now()
	out, err := runHerd(t, dir, env, "process", "--json",
		"--workspace", fakeProcessWorkspace, "--deadline", deadline.String())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("a sweep that read nothing exited 0:\n%s", out)
	}
	// One shared bound: two sequential hung reads under a per-read timer would
	// take at least 2x. The slack absorbs the build and process startup.
	if elapsed >= 2*deadline {
		t.Fatalf("sweep took %s with a %s deadline; the bound is per-read, not per-sweep", elapsed, deadline)
	}
	envelope := decodeProcessJSON(t, out)
	if !envelope.Partial {
		t.Fatalf("a deadline-truncated sweep did not report partial:\n%s", out)
	}
	if got := strings.Join(envelope.Unknowns, "; "); got == "" {
		t.Fatal("a deadline-truncated sweep reported no reason")
	}
	// The hung child is the observation subprocess and only that: the sweep
	// stops it and returns rather than detaching and waiting on it.
	if got := countCalls(processCalls(t, logPath), "pane read"); got < 1 {
		t.Fatalf("no pane read was attempted: %d", got)
	}
}

// A child that produces output forever is stopped at the byte bound. The
// refusal is explicit: a truncated flood must not be returned as pane text.
func TestProcessCLIInfiniteOutputIsBoundedAndRefused(t *testing.T) {
	dir := t.TempDir()
	env, _ := installProcessFake(t, processFakeRoster, "flood")
	buildHerd(t) // pay the build cost before the clock starts
	start := time.Now()
	out, err := runHerd(t, dir, env, "process", "--json",
		"--workspace", fakeProcessWorkspace, "--deadline", "60s")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("an unbounded flood exited 0:\n%s", out)
	}
	if elapsed >= 60*time.Second {
		t.Fatalf("the flood ran to the deadline (%s); the byte bound did not stop it", elapsed)
	}
	envelope := decodeProcessJSON(t, out)
	if !envelope.Partial {
		t.Fatalf("a refused flood was not reported as partial:\n%s", out)
	}
	if got := strings.Join(envelope.Unknowns, "; "); !strings.Contains(got, "byte bound") {
		t.Fatalf("the byte bound was not named as the reason: %q", got)
	}
	for _, item := range envelope.Items {
		if strings.Contains(item.Tail, "xxxxxxxx") {
			t.Fatalf("flood bytes were classified as pane text: %+v", item)
		}
	}
}

// --lines is user input and is validated at the CLI boundary: a negative depth
// is refused before herdr is invoked at all, and an oversized one is clamped
// rather than passed through.
func TestProcessCLIRejectsInvalidLinesAndCapsDepth(t *testing.T) {
	t.Run("negative is refused before any read", func(t *testing.T) {
		dir := t.TempDir()
		env, logPath := installProcessFake(t, processFakeRoster, "healthy")
		out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace, "--lines", "-5")
		if err == nil {
			t.Fatalf("--lines -5 was accepted:\n%s", out)
		}
		if code := exitCode(err); code != 2 {
			t.Fatalf("invalid --lines exited %d, want 2:\n%s", code, out)
		}
		if data, readErr := os.ReadFile(logPath); readErr != nil || strings.TrimSpace(string(data)) != "" {
			t.Fatalf("a rejected flag still invoked herdr: %q (%v)", data, readErr)
		}
	})

	t.Run("oversized is clamped, not passed through", func(t *testing.T) {
		dir := t.TempDir()
		env, logPath := installProcessFake(t, processFakeRoster, "healthy")
		out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace, "--lines", "100000")
		if err != nil {
			t.Fatalf("--lines 100000 should clamp, not fail: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "capped at") {
			t.Fatalf("an over-cap --lines was applied silently:\n%s", out)
		}
		calls := processCalls(t, logPath)
		for _, call := range calls {
			if strings.Contains(call, "100000") {
				t.Fatalf("the uncapped depth reached herdr: %q", call)
			}
		}
		if !strings.Contains(strings.Join(calls, "\n"), "--lines 50") {
			t.Fatalf("the clamped depth was not the one requested: %v", calls)
		}
	})

	t.Run("deadline is validated too", func(t *testing.T) {
		dir := t.TempDir()
		env, logPath := installProcessFake(t, processFakeRoster, "healthy")
		out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace, "--deadline", "-1s")
		if err == nil {
			t.Fatalf("--deadline -1s was accepted:\n%s", out)
		}
		if data, readErr := os.ReadFile(logPath); readErr != nil || strings.TrimSpace(string(data)) != "" {
			t.Fatalf("a rejected deadline still invoked herdr: %q (%v)", data, readErr)
		}
	})
}

// The scope is explicit. Without a workspace the command refuses rather than
// sweeping every fleet on the host.
func TestProcessCLIRefusesAnUnscopedSweep(t *testing.T) {
	dir := t.TempDir()
	env, logPath := installProcessFake(t, processFakeRoster, "healthy")
	out, err := runHerd(t, dir, env, "process", "--json")
	if err == nil {
		t.Fatalf("an unscoped sweep was allowed:\n%s", out)
	}
	if countCalls(processCalls(t, logPath), "agent list") != 0 {
		t.Fatal("an unscoped sweep still read the roster")
	}
}

// Every structured reply that is not a usable success must fail closed at the
// transport boundary. Each of these once had a path to being classified as a
// healthy pane: a bare error object and a truncated reply both fell through to
// "probably raw text", and an unknown result body was returned as content.
func TestProcessCLIStructuredFailuresAreUnknownNotHealthy(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
		// leak is text from the failure payload that must never appear as a
		// classified pane tail.
		leak string
	}{
		{mode: "bare-error", want: "transport error envelope", leak: "vanished"},
		{mode: "malformed", want: "structured response that could not be decoded", leak: "half"},
		{mode: "unsupported", want: "unsupported result body", leak: "payload"},
		{mode: "envelope", want: "transport error envelope", leak: "is gone"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			env, logPath := installProcessFake(t, processFakeRoster, tc.mode)
			out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace)
			if err == nil {
				t.Fatalf("a sweep that read no pane successfully exited 0:\n%s", out)
			}
			envelope := decodeProcessJSON(t, out)
			if !envelope.Partial {
				t.Fatalf("%s was not reported as partial:\n%s", tc.mode, out)
			}
			if len(envelope.Unknowns) != 2 {
				t.Fatalf("unknowns = %v, want one per failed pane", envelope.Unknowns)
			}
			if got := strings.Join(envelope.Unknowns, "; "); !strings.Contains(got, tc.want) {
				t.Fatalf("failure reason = %q, want it to contain %q", got, tc.want)
			}
			for _, item := range envelope.Items {
				if item.Class != process.Unknown {
					t.Fatalf("%s classified %s from a failed read, want UNKNOWN", item.Name, item.Class)
				}
				if strings.Contains(item.Tail, tc.leak) {
					t.Fatalf("failure payload %q leaked into the digest: %+v", tc.leak, item)
				}
				if item.Tail != "" {
					t.Fatalf("failure payload reached classification as a tail: %+v", item)
				}
			}
			// Both panes were still attempted: one bad pane does not abort the
			// sweep, it just cannot be classified.
			if got := countCalls(processCalls(t, logPath), "pane read"); got != 2 {
				t.Fatalf("pane read %d times, want 2", got)
			}
		})
	}
}

// Legacy raw text is still read, but the digest says the classification is
// unverified and refuses to call itself complete. This is the compatibility
// path made explicit rather than silently granting a healthy verdict.
//
// Against the installed herdr (0.9.0) this path never triggers: every reply is
// a {id, result} or {id, error} envelope. It exists for an older binary, and
// the sweep declines to pretend it verified anything.
func TestProcessCLILegacyRawTextIsUnverifiedNotHealthy(t *testing.T) {
	dir := t.TempDir()
	env, _ := installProcessFake(t, processFakeRoster, "rawtext")
	out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace)
	if err == nil {
		t.Fatalf("an unverified sweep exited 0:\n%s", out)
	}
	envelope := decodeProcessJSON(t, out)
	if !envelope.Partial {
		t.Fatalf("unverified pane text produced a complete digest:\n%s", out)
	}
	if got := strings.Join(envelope.Unknowns, "; "); !strings.Contains(got, "classification is unverified") {
		t.Fatalf("the ambiguity was not made explicit: %q", got)
	}
	// The text is not discarded: it is real evidence, just unproven.
	for _, item := range envelope.Items {
		if !strings.Contains(item.Tail, "PASS: 12 tests") {
			t.Fatalf("raw text was dropped instead of flagged: %+v", item)
		}
	}
}

// herdr's OWN truncation flag reaches the digest. A tail that herdr cut may be
// missing the verdict for that reason alone, and the operator is told.
func TestProcessCLIReportsHerdrsOwnTruncation(t *testing.T) {
	dir := t.TempDir()
	env, _ := installProcessFake(t, processFakeRoster, "cut")
	out, err := runHerd(t, dir, env, "process", "--json", "--workspace", fakeProcessWorkspace)
	if err == nil {
		t.Fatalf("a herdr-truncated sweep exited 0:\n%s", out)
	}
	envelope := decodeProcessJSON(t, out)
	if got := strings.Join(envelope.Unknowns, "; "); !strings.Contains(got, "herdr reported the pane tail as truncated") {
		t.Fatalf("herdr's truncation flag was dropped: %q", got)
	}
}
