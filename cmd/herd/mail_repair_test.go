package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mail"
)

// These tests cover the CLI seam only: argument handling, exit codes and the
// JSON contract. The provider's own behaviour is covered in pkg/mail, and is
// deliberately not duplicated here — an earlier version of this file called
// pkg/mail directly and so would have passed even if `herd mail repair` had
// never been wired up or had stopped parsing --act.

const cliRepairID = "cli-81751-1789141629774"

const cliLegacyRow = `{"id": "` + cliRepairID + `", "sender": "startup-fix", "recipient": "orchestrator", ` +
	`"subject": "finding", "body": "two defects", "read": false, "timestamp": "2026-09-11T10:47:09.000000-0500"}`

func cliLegacySHA() string {
	sum := sha256.Sum256([]byte(cliLegacyRow))
	return hex.EncodeToString(sum[:])
}

func cliRepairMailbox(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control-mail.jsonl")
	if err := os.WriteFile(path, []byte(cliLegacyRow+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runRepairCLI invokes the real handler with captured streams.
func runRepairCLI(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = mailRepairMain(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestMailRepairCLIUsageErrorsExitTwo(t *testing.T) {
	path := cliRepairMailbox(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no id", []string{"--mail", path}, "--id is required"},
		{"act without actor", []string{"--id", cliRepairID, "--mail", path, "--act", "--fingerprint", cliLegacySHA()}, "--actor is required"},
		{"act without fingerprint", []string{"--id", cliRepairID, "--mail", path, "--act", "--actor", "root"}, "--fingerprint is required"},
		{"unknown flag", []string{"--id", cliRepairID, "--mail", path, "--nope"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runRepairCLI(tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr: %s)", code, stderr)
			}
			if stdout != "" {
				t.Fatalf("a usage error still printed a plan: %s", stdout)
			}
			if tc.want != "" && !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr does not explain the problem: %q", stderr)
			}
		})
	}
}

// A stray positional is a typo. Ignoring it silently would let
// `mail repair --act FAC-1` read as a report-only run of something else.
func TestMailRepairCLIRejectsPositionalArguments(t *testing.T) {
	path := cliRepairMailbox(t)
	code, stdout, stderr := runRepairCLI("--id", cliRepairID, "--mail", path, "stray-arg")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "stray-arg") {
		t.Fatalf("stderr does not name the rejected argument: %q", stderr)
	}
	if stdout != "" {
		t.Fatalf("a rejected invocation still printed a plan: %s", stdout)
	}
	// And it must not have been treated as a silent report-only run.
	if _, err := os.Stat(path + ".repair.jsonl"); !os.IsNotExist(err) {
		t.Fatal("a rejected invocation touched the audit artifact")
	}
}

func TestMailRepairCLIReportOnlyEmitsPlanAndChangesNothing(t *testing.T) {
	path := cliRepairMailbox(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runRepairCLI("--id", cliRepairID, "--mail", path)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var plan mail.RepairPlan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("stdout is not a decodable plan: %v\n%s", err, stdout)
	}
	if plan.Applied {
		t.Fatal("report-only reported the repair as applied")
	}
	if plan.OriginalSHA256 != cliLegacySHA() {
		t.Fatalf("plan does not carry the fingerprint an act needs: %q", plan.OriginalSHA256)
	}
	if !strings.Contains(stderr, "REPORT ONLY") {
		t.Fatalf("report-only did not say so on stderr: %q", stderr)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("report-only mutated the mailbox")
	}
}

func TestMailRepairCLIActAppliesAndReportsApplied(t *testing.T) {
	path := cliRepairMailbox(t)
	code, stdout, stderr := runRepairCLI("--id", cliRepairID, "--mail", path,
		"--fingerprint", cliLegacySHA(), "--actor", "root", "--reason", "824", "--act")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var plan mail.RepairPlan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("stdout is not a decodable plan: %v", err)
	}
	if !plan.Applied || plan.Actor != "root" {
		t.Fatalf("act did not report an attributed application: %+v", plan)
	}
	if strings.Contains(stderr, "REPORT ONLY") {
		t.Fatal("an applied repair still printed the report-only notice")
	}
}

// A provider refusal is exit 1, distinct from a usage error's 2.
func TestMailRepairCLIProviderRefusalExitsOne(t *testing.T) {
	path := cliRepairMailbox(t)
	code, stdout, stderr := runRepairCLI("--id", cliRepairID, "--mail", path,
		"--fingerprint", strings.Repeat("0", 64), "--actor", "root", "--act")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("a refused repair still printed a plan: %s", stdout)
	}
	if !strings.Contains(stderr, "mail repair:") {
		t.Fatalf("stderr does not attribute the refusal: %q", stderr)
	}
}

// routingArgsEnv carries the child's argv as JSON. Word-splitting an env string
// would turn a mailbox path containing spaces into several flags, making this
// test depend on how the host spells its temp directory.
const routingArgsEnv = "HERD_MAIL_REPAIR_ROUTING_ARGV"

// TestMailRepairRoutingHelper is the child half of the routing test. It runs
// the real runMail dispatch, which may call os.Exit, so it must be a separate
// process or a usage exit would abort the whole suite.
func TestMailRepairRoutingHelper(t *testing.T) {
	if os.Getenv("HERD_MAIL_REPAIR_ROUTING_HELPER") != "1" {
		t.Skip("parent role: spawned by TestRunMailRoutesRepairSubcommand")
	}
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv(routingArgsEnv)), &argv); err != nil {
		t.Fatalf("child could not decode its argv: %v", err)
	}
	os.Args = append([]string{"herd", "mail", "repair"}, argv...)
	runMail()
}

// `herd mail repair` must actually be routed. Every other test here calls the
// handler directly, so all of them would still pass if the dispatch case were
// removed; this is the one that would not.
func TestRunMailRoutesRepairSubcommand(t *testing.T) {
	if os.Getenv("HERD_MAIL_REPAIR_ROUTING_HELPER") == "1" {
		t.Skip("child role")
	}
	// A directory with spaces, so argv handling is exercised rather than the
	// host's temp-path spelling.
	spaced := filepath.Join(t.TempDir(), "a mail dir")
	if err := os.MkdirAll(spaced, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spaced, "control mail.jsonl")
	if err := os.WriteFile(path, []byte(cliLegacyRow+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, " ") {
		t.Fatal("fixture precondition: the mailbox path must contain a space")
	}

	for _, tc := range []struct {
		name     string
		argv     []string
		wantCode int
	}{
		{"report-only routes and succeeds", []string{"--id", cliRepairID, "--mail", path}, 0},
		{"usage error routes and exits 2", []string{"--mail", path}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.argv)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run", "^TestMailRepairRoutingHelper$")
			cmd.Env = append(os.Environ(),
				"HERD_MAIL_REPAIR_ROUTING_HELPER=1",
				routingArgsEnv+"="+string(encoded),
			)
			out, err := cmd.CombinedOutput()
			code := 0
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("helper failed to run: %v\n%s", err, out)
			}
			if code != tc.wantCode {
				t.Fatalf("routed exit = %d, want %d\n%s", code, tc.wantCode, out)
			}
			if tc.wantCode == 0 && !strings.Contains(string(out), cliRepairID) {
				t.Fatalf("routed report did not emit a plan for the requested id:\n%s", out)
			}
		})
	}
}
