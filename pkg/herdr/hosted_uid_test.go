package herdr

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/Kampe/Herdforge/pkg/procsignal"
)

func goodCapJSON() string {
	return `{"schema_version":1,"hosted_process_uid":true,"hosted_process_gid":true,"tab_create_uid_flag":"--uid","agent_start_uid_flag":"--uid","process_lineage_proof":true}`
}

func TestNegotiateHostedUID_CurrentFleetBlocked(t *testing.T) {
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
		if strings.Contains(strings.Join(args, " "), "capabilities") {
			return goodCapJSON(), nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	cap, err := NegotiateHostedUIDCapability()
	if err != nil {
		t.Fatal(err)
	}
	if !cap.HostedProcessUID || !cap.HostedProcessGID || cap.TabCreateUIDFlag != "--uid" {
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
		{"good", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"}, true},
		{"run-as-uid", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--run-as-uid"}, true},
		{"schema0", HostedUIDCapability{SchemaVersion: 0, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"}, false},
		{"uid-false", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: false, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"}, false},
		{"gid-false", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: false, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"}, false},
		{"no-lineage", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: false, TabCreateUIDFlag: "--uid"}, false},
		{"user-flag", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--user"}, false},
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
	runHerdr = func(args ...string) (string, error) { return goodCapJSON(), nil }
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

func TestTabCreate_HostedUIDProvesAndCleansOnFail(t *testing.T) {
	// MEDIUM: TabCreate(HostedUID) must prove, not flag-and-trust.
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)

	closed := false
	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	processUIDOf = func(pid int) (int, error) { return 1, nil } // wrong uid
	readStartTok = func(pid int) (string, error) { return "tok", nil }
	signalExact = func(pid int, sig syscall.Signal) error { return nil }
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "capabilities") {
			return goodCapJSON(), nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "create" {
			return `{"result":{"tab":{"tab_id":"t1","label":"x"},"root_pane":{"pane_id":"p1","tab_id":"t1"}}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_process_group_id":900001,"foreground_processes":[{"pid":900001}]}}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "close" {
			closed = true
			return `{}`, nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	bUID := os.Getuid() + 7
	_, err := TabCreate(TabCreateOptions{Workspace: "w1", Label: "x", Cwd: "/tmp/wt", HostedUID: bUID})
	if err == nil {
		t.Fatal("expected proof failure")
	}
	if !closed {
		t.Fatal("tab must close on HostedUID proof failure")
	}
}

func TestTabCreate_AttachesNegotiatedUIDFlag(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)

	bUID := os.Getuid() + 7
	var createArgs []string
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	processUIDOf = func(pid int) (int, error) { return bUID, nil }
	readStartTok = func(pid int) (string, error) { return "tok", nil }
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "capabilities") {
			return `{"schema_version":1,"hosted_process_uid":true,"hosted_process_gid":true,"tab_create_uid_flag":"--hosted-uid","agent_start_uid_flag":"--hosted-uid","process_lineage_proof":true}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "create" {
			createArgs = append([]string{}, args...)
			return `{"result":{"tab":{"tab_id":"t1","label":"x"},"root_pane":{"pane_id":"p1","tab_id":"t1"}}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":[{"pid":900001}]}}}`, nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	if _, err := TabCreate(TabCreateOptions{Workspace: "w1", Label: "x", Cwd: "/tmp/wt", HostedUID: bUID}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(createArgs, " ")
	if !strings.Contains(joined, "--hosted-uid") || !strings.Contains(joined, fmt.Sprintf("%d", bUID)) {
		t.Fatalf("missing negotiated uid flag: %v", createArgs)
	}
}

func TestGetHostedPaneIdentity_NilForegroundFailsClosed(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	runHerdr = func(args ...string) (string, error) {
		return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":null}}}`, nil
	}
	if _, err := GetHostedPaneIdentity("p1"); err == nil {
		t.Fatal("nil foreground inventory must fail closed")
	}
}

func TestAssertHostedPaneUID_RejectsWrongShellUID(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)

	me := 424242
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":%d,"foreground_process_group_id":%d,"foreground_processes":[{"pid":%d}]}}}`, me, me, me), nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	processUIDOf = func(pid int) (int, error) { return os.Getuid(), nil }
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

func TestAssertHostedPaneUID_CoversDescendantsAndPGID(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)

	shell, child, bg := 900001, 900002, 900003
	wantUID := 501
	listPGIDMembers = func(pgid int) ([]int, error) {
		if pgid != shell {
			t.Fatalf("pgid=%d", pgid)
		}
		return []int{shell, child, bg}, nil
	}
	listChildPIDs = func(pid int) ([]int, error) {
		if pid == shell {
			return []int{child, bg}, nil
		}
		return nil, nil
	}
	runHerdr = func(args ...string) (string, error) {
		return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":%d,"foreground_process_group_id":%d,"foreground_processes":[{"pid":%d},{"pid":%d}]}}}`, shell, shell, shell, child), nil
	}
	processUIDOf = func(pid int) (int, error) { return wantUID, nil }
	readStartTok = func(pid int) (string, error) { return fmt.Sprintf("tok-%d", pid), nil }
	if err := AssertHostedPaneUID("p1", wantUID); err != nil {
		t.Fatal(err)
	}
	// Wrong UID on background descendant must fail.
	processUIDOf = func(pid int) (int, error) {
		if pid == bg {
			return wantUID + 1, nil
		}
		return wantUID, nil
	}
	if err := AssertHostedPaneUID("p1", wantUID); err == nil {
		t.Fatal("background descendant wrong uid must fail")
	}
}

func TestAssertHostedPaneUID_RejectsMissingStartToken(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)

	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	runHerdr = func(args ...string) (string, error) {
		return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":[{"pid":900001}]}}}`, nil
	}
	processUIDOf = func(pid int) (int, error) { return 501, nil }
	readStartTok = func(pid int) (string, error) { return "", fmt.Errorf("empty") }
	if err := AssertHostedPaneUID("p1", 501); err == nil {
		t.Fatal("missing start token must fail lineage proof")
	}
}

func TestSignalHostedPaneTree_RevalidatesStartToken(t *testing.T) {
	// HIGH: kill must re-read start token; reuse refuses.
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)

	fakePID := 1<<30 - 3
	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	processUIDOf = func(pid int) (int, error) { return 501, nil }
	reads := 0
	readStartTok = func(pid int) (string, error) {
		reads++
		if reads <= 1 {
			return "tok-original", nil // bind
		}
		return "tok-REUSED", nil // revalidate sees reuse
	}
	var signals []int
	signalExact = func(pid int, sig syscall.Signal) error {
		signals = append(signals, pid)
		return nil
	}
	runHerdr = func(args ...string) (string, error) {
		return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":%d,"foreground_processes":[{"pid":%d}]}}}`, fakePID, fakePID), nil
	}
	err := SignalHostedPaneTree("p1")
	if err == nil || !strings.Contains(err.Error(), "reuse") && !strings.Contains(err.Error(), "start token") {
		t.Fatalf("want pid-reuse refuse, got %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("must not signal on start-token reuse: %v", signals)
	}
}

func TestSignalHostedPaneTree_SignalsWhenTokenStable(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)

	fakePID := 1<<30 - 5
	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	processUIDOf = func(pid int) (int, error) { return 501, nil }
	readStartTok = func(pid int) (string, error) { return "stable-tok", nil }
	var signals []procsignal.KillCall
	signalExact = func(pid int, sig syscall.Signal) error {
		signals = append(signals, procsignal.KillCall{PID: pid, Sig: sig})
		return nil
	}
	runHerdr = func(args ...string) (string, error) {
		return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":%d,"foreground_processes":[{"pid":%d}]}}}`, fakePID, fakePID), nil
	}
	if err := SignalHostedPaneTree("p1"); err != nil {
		t.Fatal(err)
	}
	if len(signals) < 2 {
		t.Fatalf("expected TERM+KILL, got %v", signals)
	}
	for _, c := range signals {
		if c.PID <= 1 || c.PID < 0 {
			t.Fatalf("unsafe signal target: %+v", c)
		}
	}
}

func TestFailHostedIsolationProof_ClosesTab(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)

	fakePID := 1<<30 - 1
	closed := false
	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	processUIDOf = func(pid int) (int, error) { return 501, nil }
	readStartTok = func(pid int) (string, error) { return "tok", nil }
	signalExact = func(pid int, sig syscall.Signal) error { return nil }
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
}

func TestTerminateHostedPaneTree_NeverSignalsSelf(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)

	self := 777
	osGetpid = func() int { return self }
	sleepBrief = func() {}
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
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
}

func TestHostedUIDIsolationRequired_ForceAndEnv(t *testing.T) {
	me := os.Getuid()
	t.Setenv(EnvBuilderUID, "")
	t.Setenv(EnvRequireHostedUID, "")
	if HostedUIDIsolationRequired() {
		t.Fatal("empty env must not require isolation")
	}
	// Force-on even when builder == coordinator (gate fails closed later).
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me))
	t.Setenv(EnvRequireHostedUID, "1")
	if !HostedUIDIsolationRequired() {
		t.Fatal("HERD_REQUIRE_HOSTED_UID=1 must force isolation path")
	}
	t.Setenv(EnvRequireHostedUID, "")
	if HostedUIDIsolationRequired() {
		t.Fatal("builder == self without force is not isolation")
	}
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me+1))
	if !HostedUIDIsolationRequired() {
		t.Fatal("distinct builder uid must require isolation")
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

func TestMutation_ValidateCapabilityGatesRemovedWouldFail(t *testing.T) {
	good := HostedUIDCapability{
		SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid",
	}
	if err := validateHostedUIDCapability(good); err != nil {
		t.Fatal(err)
	}
	mutants := []HostedUIDCapability{
		{SchemaVersion: 0, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: false, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: false, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: false, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: ""},
		{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--user"},
	}
	for i, m := range mutants {
		if err := validateHostedUIDCapability(m); err == nil {
			t.Fatalf("mutant %d accepted — capability gate is vacuous", i)
		}
	}
}

func TestAssertAgentHostedAsBuilder_ExactRoutedOwner(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { processUIDOf = old }(processUIDOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) (int, error)) { readParent = old }(readParent)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)

	builder := 501
	argv := []string{"codex", "--model", "gpt-5.6-luna"}
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	processUIDOf = func(pid int) (int, error) { return builder, nil }
	readStartTok = func(pid int) (string, error) { return fmt.Sprintf("tok-%d", pid), nil }
	readParent = func(pid int) (int, error) {
		if pid == 501 {
			return 500, nil
		}
		return 1, nil
	}
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","agent":"codex","pane_id":"p1","tab_id":"t1"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"pane_id":"p1","shell_pid":499,"foreground_processes":[
				{"pid":499,"name":"zsh","argv":["zsh"]},
				{"pid":500,"name":"node","argv":["node","/opt/homebrew/bin/codex"]},
				{"pid":501,"name":"codex","argv":["codex","--model","gpt-5.6-luna"]}
			]}}}`, nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	if err := AssertAgentHostedAsBuilder("worker", builder, "codex", argv); err != nil {
		t.Fatal(err)
	}
	// Wrong agent uid.
	processUIDOf = func(pid int) (int, error) {
		if pid == 501 {
			return builder + 1, nil
		}
		return builder, nil
	}
	if err := AssertAgentHostedAsBuilder("worker", builder, "codex", argv); err == nil {
		t.Fatal("wrong agent uid must fail")
	}
}

func errorsIsHostedBlocked(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "FAC-172") || strings.Contains(s, "BLOCKED") || strings.Contains(s, "hosted process UID")
}
