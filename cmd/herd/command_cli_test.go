package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/cmdauth"
)

// FAC-195 at the binary boundary. Unlike the package tests, every attempt
// here is a SEPARATE OS PROCESS with its own ledger handle — which is what
// the incident actually looked like (a worker shelling out again after an
// edit) and what "atomic across competing workers/processes" has to mean.

type cmdEnv struct {
	binary string
	db     string
	work   string
	marker string
}

// newCmdEnv gives each scenario its own ledger, worktree, and marker file
// while sharing one built binary — cmd/herd is expensive to link and this
// package's suite already runs close to its time budget.
func newCmdEnv(t *testing.T, binary string) cmdEnv {
	t.Helper()
	return cmdEnv{
		binary: binary,
		db:     filepath.Join(t.TempDir(), "cmdauth.db"),
		work:   t.TempDir(),
		marker: filepath.Join(t.TempDir(), "executions.log"),
	}
}

func (e cmdEnv) run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	c := exec.Command(e.binary, args...)
	c.Dir = e.work
	out, err := c.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("herd %v: %v\n%s", args, err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

// guardedArgv is the command under authorization: it appends one line to the
// marker file and then fails, standing in for FAC-151's failing guarded test.
func (e cmdEnv) guardedArgv() []string {
	return []string{"sh", "-c", "echo ran >> " + e.marker + "; exit 1"}
}

func (e cmdEnv) executions(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(e.marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// cliExactlyOneExecutionAcrossProcesses is the acceptance
// criterion end to end: four separate `herd command run` processes against one
// single-attempt stop-on-first-failure authorization; one executes, three are
// refused before any child process exists.
func cliExactlyOneExecutionAcrossProcesses(t *testing.T, binary string) {
	e := newCmdEnv(t, binary)
	argv := e.guardedArgv()

	authorizeArgs := append([]string{"command", "authorize", "--db", e.db,
		"--id", "FAC151-C1", "--lane", "worker-a", "--session", "S-019fcb4f",
		"--authority", "root", "--max-attempts", "1",
		"--disposition", "stop-on-first-failure", "--dir", e.work, "--"}, argv...)
	if out, code := e.run(t, authorizeArgs...); code != 0 {
		t.Fatalf("authorize exit %d:\n%s", code, out)
	}

	runArgs := append([]string{"command", "run", "--db", e.db,
		"--id", "FAC151-C1", "--lane", "worker-a", "--session", "S-019fcb4f",
		"--dir", e.work, "--"}, argv...)

	// Attempt 1: permitted, runs, exits 1.
	out, code := e.run(t, runArgs...)
	if code != 1 {
		t.Fatalf("first attempt exit %d, want the command's own 1:\n%s", code, out)
	}

	// Attempts 2-4: the worker edits and retries, each in a fresh process.
	for attempt := 2; attempt <= 4; attempt++ {
		if err := os.WriteFile(filepath.Join(e.work, "repair.go"), []byte("// fix\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, code := e.run(t, runArgs...)
		if code != exitRefused {
			t.Fatalf("attempt %d exit %d, want %d (refused):\n%s", attempt, code, exitRefused, out)
		}
		if !strings.Contains(out, "REFUSED") {
			t.Fatalf("attempt %d did not report a refusal:\n%s", attempt, out)
		}
	}

	if n := e.executions(t); n != 1 {
		t.Fatalf("the guarded command ran %d times, want exactly 1", n)
	}

	// Durable readback names every attempt and its reason.
	out, code = e.run(t, "command", "receipts", "--db", e.db, "--id", "FAC151-C1", "--json")
	if code != 0 {
		t.Fatalf("receipts exit %d:\n%s", code, out)
	}
	var receipts []cmdauth.Receipt
	if err := json.Unmarshal([]byte(out), &receipts); err != nil {
		t.Fatalf("receipts json: %v\n%s", err, out)
	}
	counts := map[string]int{}
	for _, r := range receipts {
		counts[r.Event]++
		if r.Lane != "worker-a" || r.SessionID != "S-019fcb4f" {
			t.Fatalf("receipt lost lane/session identity: %+v", r)
		}
	}
	if counts[cmdauth.EventConsumed] != 1 || counts[cmdauth.EventFailed] != 1 || counts[cmdauth.EventRejected] != 3 {
		t.Fatalf("receipt tally = %v, want 1 consumed / 1 failed / 3 rejected", counts)
	}
}

// cliOnlyANewCommandIDReopensExecution: after the burn, a fresh
// root authorization with a DISTINCT id is the only thing that runs again.
func cliOnlyANewCommandIDReopensExecution(t *testing.T, binary string) {
	e := newCmdEnv(t, binary)
	argv := e.guardedArgv()
	base := []string{"command", "authorize", "--db", e.db, "--lane", "worker-a",
		"--session", "S1", "--authority", "root", "--max-attempts", "1",
		"--disposition", "stop-on-first-failure", "--dir", e.work}

	if out, code := e.run(t, append(append([]string{}, append(base, "--id", "C1", "--")...), argv...)...); code != 0 {
		t.Fatalf("authorize C1: %d\n%s", code, out)
	}
	runC1 := append([]string{"command", "run", "--db", e.db, "--id", "C1",
		"--lane", "worker-a", "--session", "S1", "--dir", e.work, "--"}, argv...)
	if _, code := e.run(t, runC1...); code != 1 {
		t.Fatal("first run should have executed and failed")
	}
	if _, code := e.run(t, runC1...); code != exitRefused {
		t.Fatal("replay of the burned token must be refused")
	}

	// Re-issuing the SAME id changes nothing (it is an idempotent replay).
	if out, code := e.run(t, append(append([]string{}, append(base, "--id", "C1", "--")...), argv...)...); code != 0 {
		t.Fatalf("re-authorize C1: %d\n%s", code, out)
	}
	if _, code := e.run(t, runC1...); code != exitRefused {
		t.Fatal("re-delivering the same authorization must not reopen it")
	}
	if n := e.executions(t); n != 1 {
		t.Fatalf("executions=%d, want 1", n)
	}

	// A distinct command id from root does reopen it.
	if out, code := e.run(t, append(append([]string{}, append(base, "--id", "C2", "--")...), argv...)...); code != 0 {
		t.Fatalf("authorize C2: %d\n%s", code, out)
	}
	runC2 := append([]string{"command", "run", "--db", e.db, "--id", "C2",
		"--lane", "worker-a", "--session", "S1", "--dir", e.work, "--"}, argv...)
	if _, code := e.run(t, runC2...); code != 1 {
		t.Fatal("a distinct newly authorized command id must be permitted")
	}
	if n := e.executions(t); n != 2 {
		t.Fatalf("executions=%d, want 2", n)
	}
}

// cliRefusesUnauthorizedArgv: the argv actually spawned is what
// gets hashed, so an authorization cannot be spent on a different command.
func cliRefusesUnauthorizedArgv(t *testing.T, binary string) {
	e := newCmdEnv(t, binary)
	authorized := []string{"sh", "-c", "echo ran >> " + e.marker}
	smuggled := []string{"sh", "-c", "echo smuggled >> " + e.marker}

	if out, code := e.run(t, append([]string{"command", "authorize", "--db", e.db,
		"--id", "C1", "--lane", "l", "--session", "s", "--authority", "root",
		"--max-attempts", "1", "--disposition", "continue-on-failure",
		"--dir", e.work, "--"}, authorized...)...); code != 0 {
		t.Fatalf("authorize: %d\n%s", code, out)
	}
	out, code := e.run(t, append([]string{"command", "run", "--db", e.db, "--id", "C1",
		"--lane", "l", "--session", "s", "--dir", e.work, "--"}, smuggled...)...)
	if code != exitRefused {
		t.Fatalf("smuggled argv exit %d, want %d:\n%s", code, exitRefused, out)
	}
	if n := e.executions(t); n != 0 {
		t.Fatalf("a refused attempt executed %d times", n)
	}
}

// cliConcurrentProcessesRaceForOneAttempt: eight real processes
// start at once against a single-attempt budget. Exactly one may run.
func cliConcurrentProcessesRaceForOneAttempt(t *testing.T, binary string) {
	e := newCmdEnv(t, binary)
	argv := []string{"sh", "-c", "echo ran >> " + e.marker}

	if out, code := e.run(t, append([]string{"command", "authorize", "--db", e.db,
		"--id", "C1", "--lane", "l", "--session", "s", "--authority", "root",
		"--max-attempts", "1", "--disposition", "continue-on-failure",
		"--dir", e.work, "--"}, argv...)...); code != 0 {
		t.Fatalf("authorize: %d\n%s", code, out)
	}
	runArgs := append([]string{"command", "run", "--db", e.db, "--id", "C1",
		"--lane", "l", "--session", "s", "--dir", e.work, "--"}, argv...)

	const workers = 8
	var wg sync.WaitGroup
	codes := make(chan int, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c := exec.Command(e.binary, runArgs...)
			c.Dir = e.work
			out, err := c.CombinedOutput()
			if err == nil {
				codes <- 0
				return
			}
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Errorf("run: %v\n%s", err, out)
				codes <- -1
				return
			}
			codes <- ee.ExitCode()
		}()
	}
	close(start)
	wg.Wait()
	close(codes)

	granted, refused := 0, 0
	for c := range codes {
		switch c {
		case 0:
			granted++
		case exitRefused:
			refused++
		default:
			t.Fatalf("unexpected exit code %d", c)
		}
	}
	if granted != 1 || refused != workers-1 {
		t.Fatalf("granted=%d refused=%d, want 1 and %d", granted, refused, workers-1)
	}
	if n := e.executions(t); n != 1 {
		t.Fatalf("executions=%d, want exactly 1", n)
	}
}

// cliRejectsMalformedAuthorizations keeps the issuing side honest:
// no wildcards, no defaults that quietly widen a budget.
func cliRejectsMalformedAuthorizations(t *testing.T, binary string) {
	e := newCmdEnv(t, binary)
	argv := []string{"true"}
	base := []string{"command", "authorize", "--db", e.db, "--id", "C1",
		"--lane", "l", "--session", "s", "--authority", "root",
		"--max-attempts", "1", "--disposition", "stop-on-first-failure", "--dir", e.work}

	drop := func(flagName string) []string {
		var out []string
		for i := 0; i < len(base); i++ {
			if base[i] == flagName {
				i++
				continue
			}
			out = append(out, base[i])
		}
		return append(out, "--", argv[0])
	}
	for _, flagName := range []string{"--id", "--lane", "--session", "--authority"} {
		if out, code := e.run(t, drop(flagName)...); code == 0 {
			t.Fatalf("missing %s was accepted:\n%s", flagName, out)
		}
	}
	// No argv after `--`.
	if out, code := e.run(t, base...); code == 0 {
		t.Fatalf("missing argv was accepted:\n%s", out)
	}
	// Unknown disposition.
	bad := append(append([]string{}, base...), "--", argv[0])
	for i := range bad {
		if bad[i] == "stop-on-first-failure" {
			bad[i] = "retry-until-green"
		}
	}
	if out, code := e.run(t, bad...); code == 0 {
		t.Fatalf("unknown disposition was accepted:\n%s", out)
	}
	// Zero attempts is not an unlimited budget.
	zero := append(append([]string{}, base...), "--", argv[0])
	for i := range zero {
		if zero[i] == "--max-attempts" {
			zero[i+1] = "0"
		}
	}
	if out, code := e.run(t, zero...); code == 0 {
		t.Fatalf("--max-attempts 0 was accepted:\n%s", out)
	}
}

// cliUnknownActionIsARefusal keeps the boundary fail-closed at the
// dispatch layer too.
func cliUnknownActionIsARefusal(t *testing.T, binary string) {
	e := newCmdEnv(t, binary)
	if out, code := e.run(t, "command", "exec-anyway"); code == 0 {
		t.Fatalf("unknown action accepted:\n%s", out)
	}
	if out, code := e.run(t, "command"); code == 0 {
		t.Fatalf("bare `herd command` accepted:\n%s", out)
	}
}

// TestCommandCLI builds the binary once and runs every FAC-195 boundary
// scenario against it.
func TestCommandCLI(t *testing.T) {
	binary := buildHerd(t)
	for name, fn := range map[string]func(*testing.T, string){
		"ExactlyOneExecutionAcrossProcesses": cliExactlyOneExecutionAcrossProcesses,
		"OnlyANewCommandIDReopensExecution":  cliOnlyANewCommandIDReopensExecution,
		"RefusesUnauthorizedArgv":            cliRefusesUnauthorizedArgv,
		"ConcurrentProcessesRaceForOne":      cliConcurrentProcessesRaceForOneAttempt,
		"RejectsMalformedAuthorizations":     cliRejectsMalformedAuthorizations,
		"UnknownActionIsARefusal":            cliUnknownActionIsARefusal,
	} {
		name, fn := name, fn
		t.Run(name, func(t *testing.T) { fn(t, binary) })
	}
}
