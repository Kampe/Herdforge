package herdr

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/Kampe/Herdforge/pkg/procsignal"
)

func TestNegotiateHostedUID_CurrentFleetBlocked(t *testing.T) {
	// Live herdr has no api capabilities — FAC-172 blocked until daemon ships.
	_, err := NegotiateHostedUIDCapability()
	if err == nil {
		t.Fatal("current fleet must not report hosted-uid capability without FAC-172 API")
	}
	if !errorsIsHostedBlocked(err) {
		t.Fatalf("want FAC-172/BLOCKED, got: %v", err)
	}
	if HerdrSupportsHostedUID() {
		t.Fatal("HerdrSupportsHostedUID must be false on current fleet")
	}
}

func TestNegotiateHostedUID_NoHelpTextSpoof(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "capabilities") {
			return "", fmt.Errorf("exit 1: unknown command")
		}
		if strings.Contains(joined, "--help") {
			return "Usage: herdr tab create --uid <UID>\n", nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	if HerdrSupportsHostedUID() {
		t.Fatal("help-text --uid must not grant capability")
	}
	if err := RequireHerdrBuilderSpawnCapability(os.Getuid() + 1); err == nil {
		t.Fatal("expected BLOCKED")
	}
}

func TestNegotiateHostedUID_StructuredJSONUnlocks(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "capabilities") {
			return `{"schema_version":1,"hosted_process_uid":true,"hosted_process_gid":true,"tab_create_uid_flag":"--uid","agent_start_uid_flag":"--uid","process_lineage_proof":true}`, nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	cap, err := NegotiateHostedUIDCapability()
	if err != nil {
		t.Fatal(err)
	}
	if !cap.HostedProcessUID || cap.TabCreateUIDFlag != "--uid" {
		t.Fatalf("bad cap: %+v", cap)
	}
	if err := RequireHerdrBuilderSpawnCapability(os.Getuid() + 1); err != nil {
		t.Fatal(err)
	}
}

func TestValidateHostedUIDCapability_Table(t *testing.T) {
	cases := []struct {
		name string
		cap  HostedUIDCapability
		ok   bool
	}{
		{"good", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"}, true},
		{"run-as-uid", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--run-as-uid"}, true},
		{"hosted-uid", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--hosted-uid"}, true},
		{"schema0", HostedUIDCapability{SchemaVersion: 0, HostedProcessUID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"}, false},
		{"uid-false", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: false, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"}, false},
		{"no-lineage", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, ProcessLineageProof: false, TabCreateUIDFlag: "--uid"}, false},
		{"user-flag", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--user"}, false},
		{"bad-agent-flag", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid", AgentStartUIDFlag: "--user"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHostedUIDCapability(tc.cap)
			if tc.ok && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestRequireCapability_RejectsCoordinatorUID(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	runHerdr = func(args ...string) (string, error) {
		return `{"schema_version":1,"hosted_process_uid":true,"process_lineage_proof":true,"tab_create_uid_flag":"--uid"}`, nil
	}
	if err := RequireHerdrBuilderSpawnCapability(os.Getuid()); err == nil {
		t.Fatal("builder == coordinator must be rejected")
	}
	if err := RequireHerdrBuilderSpawnCapability(0); err == nil {
		t.Fatal("uid 0 must be rejected")
	}
}

func TestTabCreateForTask_BlocksWithoutHostedUIDCapability(t *testing.T) {
	me := os.Getuid()
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me+1))
	t.Setenv(EnvRequireHostedUID, "")

	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "capabilities") {
			return "", fmt.Errorf("unknown command")
		}
		t.Fatalf("must not create tab without capability; args=%v", args)
		return "", nil
	}

	_, err := TabCreateForTask("w1", "task-fac-1", "/tmp/wt", true)
	if err == nil {
		t.Fatal("expected BLOCKED without herdr hosted-uid capability")
	}
	if !errorsIsHostedBlocked(err) {
		t.Fatalf("want FAC-172/BLOCKED, got: %v", err)
	}
}

func TestTabCreateForTask_WithoutIsolationEnvStillWorks(t *testing.T) {
	t.Setenv(EnvBuilderUID, "")
	t.Setenv(EnvRequireHostedUID, "")
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	var got []string
	runHerdr = func(args ...string) (string, error) {
		got = append([]string{}, args...)
		return `{"result":{"tab":{"tab_id":"t1","label":"task-fac-1"},"root_pane":{"pane_id":"p1","tab_id":"t1"}}}`, nil
	}
	tab, err := TabCreateForTask("wABC", "task-fac-1", "/repo/.herd/worktrees/fac-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if tab.ID != "t1" {
		t.Fatalf("tab=%+v", tab)
	}
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "--uid") {
		t.Fatalf("must not attach uid flags without isolation: %v", got)
	}
}

func TestTabCreate_AttachesNegotiatedUIDFlag(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	var got []string
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "capabilities") {
			return `{"schema_version":1,"hosted_process_uid":true,"process_lineage_proof":true,"tab_create_uid_flag":"--hosted-uid","agent_start_uid_flag":"--hosted-uid"}`, nil
		}
		got = append([]string{}, args...)
		return `{"result":{"tab":{"tab_id":"t1","label":"x"},"root_pane":{"pane_id":"p1","tab_id":"t1"}}}`, nil
	}
	bUID := os.Getuid() + 7
	_, err := TabCreate(TabCreateOptions{Workspace: "w1", Label: "x", Cwd: "/tmp/wt", HostedUID: bUID})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--hosted-uid") || !strings.Contains(joined, fmt.Sprintf("%d", bUID)) {
		t.Fatalf("missing negotiated uid flag: %v", got)
	}
}

func TestAssertHostedPaneUID_RejectsWrongShellUID(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)

	me := 424242
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":%d,"foreground_process_group_id":%d,"foreground_processes":[{"pid":%d}]}}}`, me, me, me), nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	processUIDOf = func(pid int) (int, error) { return os.Getuid(), nil } // coordinator uid
	readStartTok = func(pid int) (string, error) { return "start-token", nil }

	want := os.Getuid() + 999
	err := AssertHostedPaneUID("p1", want)
	if err == nil {
		t.Fatal("must reject shell uid != BuilderUID")
	}
	if !strings.Contains(err.Error(), "CLI setuid is not isolation") {
		t.Fatalf("want negative-control wording, got: %v", err)
	}
}

func TestAssertHostedPaneUID_AcceptsMatchingUIDWithLineage(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)

	shell, child := 900001, 900002
	wantUID := 501
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":%d,"foreground_process_group_id":%d,"foreground_processes":[{"pid":%d},{"pid":%d}]}}}`, shell, shell, shell, child), nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	processUIDOf = func(pid int) (int, error) { return wantUID, nil }
	readStartTok = func(pid int) (string, error) {
		return fmt.Sprintf("tok-%d", pid), nil
	}
	if err := AssertHostedPaneUID("p1", wantUID); err != nil {
		t.Fatal(err)
	}
}

func TestAssertHostedPaneUID_RejectsMissingStartToken(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)

	runHerdr = func(args ...string) (string, error) {
		return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":[{"pid":900001}]}}}`, nil
	}
	processUIDOf = func(pid int) (int, error) { return 501, nil }
	readStartTok = func(pid int) (string, error) { return "", fmt.Errorf("empty") }
	if err := AssertHostedPaneUID("p1", 501); err == nil {
		t.Fatal("missing start token must fail lineage proof")
	}
}

func TestFailHostedIsolationProof_SignalsExactPIDsAndClosesTab(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)

	fakePID := 1<<30 - 3
	closed := false
	var signals []procsignal.KillCall
	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
	processUIDOf = func(pid int) (int, error) { return 501, nil }
	readStartTok = func(pid int) (string, error) { return "tok", nil }
	signalExact = func(pid int, sig syscall.Signal) error {
		signals = append(signals, procsignal.KillCall{PID: pid, Sig: sig})
		return nil
	}
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":%d,"foreground_processes":[{"pid":%d}]}}}`, fakePID, fakePID), nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "close" {
			closed = true
			return `{}`, nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}

	err := FailHostedIsolationProof("p1", "t1", fmt.Errorf("proof failed"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !closed {
		t.Fatal("tab must be closed on isolation proof failure")
	}
	if len(signals) == 0 {
		t.Fatal("expected exact-pid signals")
	}
	for _, c := range signals {
		if c.PID <= 1 {
			t.Fatalf("must never signal host-wide/sentinel target: %+v", c)
		}
		if c.PID == -1 || c.PID < 0 {
			t.Fatalf("must never issue process-group broadcast kill: %+v", c)
		}
	}
}

func TestTerminateHostedPaneTree_NeverSignalsSelf(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)

	self := 777
	osGetpid = func() int { return self }
	sleepBrief = func() {}
	processUIDOf = func(pid int) (int, error) { return 501, nil }
	readStartTok = func(pid int) (string, error) { return "tok", nil }
	var signals []int
	signalExact = func(pid int, sig syscall.Signal) error {
		signals = append(signals, pid)
		return nil
	}
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":%d,"foreground_processes":[{"pid":%d},{"pid":%d}]}}}`, self, self, 888), nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "close" {
			return `{}`, nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	_ = TerminateHostedPaneTree("p1", "t1")
	for _, pid := range signals {
		if pid == self {
			t.Fatal("must never signal self")
		}
	}
	if len(signals) != 2 { // TERM+KILL on 888 only
		t.Fatalf("signals=%v want only pid 888", signals)
	}
}

func TestNoCLISetuidEnforcementInSource(t *testing.T) {
	for _, name := range []string{"herdr.go", "hosted_uid.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		if strings.Contains(src, "runHerdrAsUID") || strings.Contains(src, "syscall.Setuid") || strings.Contains(src, "Credential{Uid") {
			t.Fatalf("%s must not setuid the herdr CLI as FAC-172 isolation", name)
		}
		if strings.Contains(src, `strings.Contains(out, "--uid")`) || strings.Contains(src, `tab", "create", "--help"`) {
			t.Fatalf("%s must not scrape help text for --uid capability", name)
		}
		if strings.Contains(src, "syscall.Kill(-1") || strings.Contains(src, "Kill(-1,") {
			t.Fatalf("%s must not issue kill(-1) host-wide broadcast", name)
		}
	}
}

func TestHostedUIDIsolationRequired_Env(t *testing.T) {
	me := os.Getuid()
	t.Setenv(EnvBuilderUID, "")
	t.Setenv(EnvRequireHostedUID, "")
	if HostedUIDIsolationRequired() {
		t.Fatal("empty env must not require isolation")
	}
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me))
	if HostedUIDIsolationRequired() {
		t.Fatal("builder == self is not isolation")
	}
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me+1))
	if !HostedUIDIsolationRequired() {
		t.Fatal("distinct builder uid must require isolation")
	}
}

func TestMutation_ValidateCapabilityGatesRemovedWouldFail(t *testing.T) {
	// Non-vacuous: each required field, when stripped, must turn RED.
	good := HostedUIDCapability{
		SchemaVersion: 1, HostedProcessUID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid",
	}
	if err := validateHostedUIDCapability(good); err != nil {
		t.Fatal(err)
	}
	mutants := []HostedUIDCapability{
		{SchemaVersion: 0, HostedProcessUID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: false, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: true, ProcessLineageProof: false, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: true, ProcessLineageProof: true, TabCreateUIDFlag: ""},
		{SchemaVersion: 1, HostedProcessUID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--user"},
	}
	for i, m := range mutants {
		if err := validateHostedUIDCapability(m); err == nil {
			t.Fatalf("mutant %d accepted — capability gate is vacuous", i)
		}
	}
}

func errorsIsHostedBlocked(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "FAC-172") || strings.Contains(s, "BLOCKED") || strings.Contains(s, "hosted process UID")
}
