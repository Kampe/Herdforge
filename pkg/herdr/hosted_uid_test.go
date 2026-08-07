package herdr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/procsignal"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)


func goodCapJSON() string {
	return `{"schema_version":1,"hosted_process_uid":true,"hosted_process_gid":true,"tab_create_uid_flag":"--uid","agent_start_uid_flag":"--uid","process_lineage_proof":true}`
}

func pinBuilderGID(t *testing.T, gid int) {
	t.Helper()
	t.Setenv(EnvBuilderGID, fmt.Sprintf("%d", gid))
}

func stubHostedTreeSeams(t *testing.T, uid, gid int) {
	t.Helper()
	pinBuilderGID(t, gid)
	ResetHostedUIDCapabilityCache()
	t.Cleanup(ResetHostedUIDCapabilityCache)
	oldCreds := processCredsOf
	oldTok, oldPG, oldKids := readStartTok, listPGIDMembers, listChildPIDs
	oldExists := processExists
	oldReadySleep, oldReadyTries := agentReadySleep, agentReadyTries
	t.Cleanup(func() {
		processCredsOf = oldCreds
		readStartTok, listPGIDMembers, listChildPIDs = oldTok, oldPG, oldKids
		processExists = oldExists
		agentReadySleep, agentReadyTries = oldReadySleep, oldReadyTries
	})
	processCredsOf = func(int) (processCreds, error) {
		return processCreds{RUID: uid, EUID: uid, RGID: gid, EGID: gid}, nil
	}
	readStartTok = func(pid int) (string, error) { return fmt.Sprintf("tok-%d", pid), nil }
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	processExists = func(int) bool { return true }
	// Unit tests do not wait on async agent inventory unless they opt in.
	agentReadyTries = 1
	agentReadySleep = func() {}
}

func TestNegotiateHostedUID_CurrentFleetBlocked(t *testing.T) {
	// Stub only — never depend on a live herdr binary or ambient daemon.
	ResetHostedUIDCapabilityCache()
	t.Cleanup(ResetHostedUIDCapabilityCache)
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	runHerdr = func(args ...string) (string, error) {
		// Real fleet today: exit 0 with usage text (not JSON).
		return "herdr api commands:\n  herdr api snapshot\n  herdr api schema\n", nil
	}
	_, err := NegotiateHostedUIDCapability()
	if err == nil {
		t.Fatal("usage-text capabilities output must not unlock isolation")
	}
	if !errorsIsHostedBlocked(err) {
		t.Fatalf("want FAC-172/BLOCKED, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unreadable") && !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("want JSON-unreadable cause in error, got: %v", err)
	}
	// Command-failure path preserves underlying cause.
	ResetHostedUIDCapabilityCache()
	runHerdr = func(args ...string) (string, error) {
		return "", fmt.Errorf("exit 1: unknown command")
	}
	_, err = NegotiateHostedUIDCapability()
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("want underlying cause preserved, got %v", err)
	}
}

func TestNegotiateHostedUID_NoHelpTextSpoof(t *testing.T) {
	ResetHostedUIDCapabilityCache()
	t.Cleanup(ResetHostedUIDCapabilityCache)
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
	ResetHostedUIDCapabilityCache()
	t.Cleanup(ResetHostedUIDCapabilityCache)
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	calls := 0
	runHerdr = func(args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "capabilities") {
			calls++
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
	// Cache: second negotiate must not re-shell.
	if _, err := NegotiateHostedUIDCapability(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("capability negotiation not cached: calls=%d", calls)
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
		{"gid-false", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: false, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"}, false},
		{"uid-false", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: false, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"}, false},
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
	ResetHostedUIDCapabilityCache()
	t.Cleanup(ResetHostedUIDCapabilityCache)
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
	ResetHostedUIDCapabilityCache()
	t.Cleanup(ResetHostedUIDCapabilityCache)
	me := os.Getuid()
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me+1))
	t.Setenv(EnvRequireHostedUID, "")
	wt := t.TempDir()
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	runHerdr = func(args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "capabilities") {
			return "", fmt.Errorf("unknown command")
		}
		t.Fatalf("must not create tab without capability; args=%v", args)
		return "", nil
	}
	_, err := TabCreateForTask("w1", "task-fac-1", wt, true)
	if err == nil || !errorsIsHostedBlocked(err) {
		t.Fatalf("want FAC-172 BLOCKED, got %v", err)
	}
}

func TestTabCreateForTask_WithoutIsolationEnvStillWorks(t *testing.T) {
	t.Setenv(EnvBuilderUID, "")
	t.Setenv(EnvRequireHostedUID, "")
	wt := t.TempDir()
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	var got []string
	runHerdr = func(args ...string) (string, error) {
		got = append([]string{}, args...)
		return `{"result":{"tab":{"tab_id":"t1","label":"task-fac-1"},"root_pane":{"pane_id":"p1","tab_id":"t1"}}}`, nil
	}
	tab, err := TabCreateForTask("wABC", "task-fac-1", wt, true)
	if err != nil {
		t.Fatal(err)
	}
	if tab.ID != "t1" {
		t.Fatalf("tab=%+v", tab)
	}
	if strings.Contains(strings.Join(got, " "), "--uid") {
		t.Fatalf("must not attach uid flags without isolation: %v", got)
	}
}

func TestTabCreate_HostedUIDProvesAndCleansOnFail(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)
	stubHostedTreeSeams(t, 1, 99) // wrong uid vs HostedUID
	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
	var signals []procsignal.KillCall
	signalExact = func(pid int, sig syscall.Signal) error {
		signals = append(signals, procsignal.KillCall{PID: pid, Sig: sig})
		return nil
	}
	closed := false
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "capabilities") {
			return goodCapJSON(), nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "create" {
			return `{"result":{"tab":{"tab_id":"t1","label":"x"},"root_pane":{"pane_id":"p1","tab_id":"t1"}}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":[{"pid":900001}]}}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "close" {
			closed = true
			return `{}`, nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	bUID := os.Getuid() + 7
	if _, err := TabCreate(TabCreateOptions{Workspace: "w1", Label: "x", Cwd: "/tmp/wt", HostedUID: bUID}); err == nil {
		t.Fatal("expected proof failure")
	}
	if !closed {
		t.Fatal("tab must close on HostedUID proof failure")
	}
	if len(signals) == 0 {
		t.Fatal("proof failure must signal isolation tree")
	}
}

func TestTabCreate_AttachesNegotiatedUIDFlag(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	bUID := os.Getuid() + 7
	stubHostedTreeSeams(t, bUID, 42)
	var createArgs []string
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

func TestGetHostedPaneIdentity_UnsafePGIDFailsClosed(t *testing.T) {
	// LOW residual: ValidatePGID failure must not silently skip PG expansion.
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	stubHostedTreeSeams(t, 501, 42)
	callerPG := syscall.Getpgrp()
	runHerdr = func(args ...string) (string, error) {
		// Report the caller's own process group — ValidatePGID refuses it.
		return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_process_group_id":%d,"foreground_processes":[{"pid":900001}]}}}`, callerPG), nil
	}
	if _, err := GetHostedPaneIdentity("p1"); err == nil || !strings.Contains(err.Error(), "process group") {
		t.Fatalf("want fail-closed on unsafe pgid, got %v", err)
	}
}

func TestAssertHostedPaneUID_RejectsWrongShellUID(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	stubHostedTreeSeams(t, os.Getuid(), 42) // coordinator uid, not builder
	me := 424242
	runHerdr = func(args ...string) (string, error) {
		return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":%d,"foreground_processes":[{"pid":%d}]}}}`, me, me), nil
	}
	_, err := AssertHostedPaneUID("p1", os.Getuid()+999)
	if err == nil || !strings.Contains(err.Error(), "BuilderUID") {
		t.Fatalf("want BuilderUID mismatch, got: %v", err)
	}
}

func TestAssertHostedPaneUID_RejectsWrongGID(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	stubHostedTreeSeams(t, 501, 42)
	processCredsOf = func(pid int) (processCreds, error) {
		if pid == 900002 {
			return processCreds{RUID: 501, EUID: 501, RGID: 99, EGID: 99}, nil
		}
		return processCreds{RUID: 501, EUID: 501, RGID: 42, EGID: 42}, nil
	}
	listChildPIDs = func(pid int) ([]int, error) {
		if pid == 900001 {
			return []int{900002}, nil
		}
		return nil, nil
	}
	runHerdr = func(args ...string) (string, error) {
		return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":[{"pid":900001}]}}}`, nil
	}
	if _, err := AssertHostedPaneUID("p1", 501); err == nil || !strings.Contains(err.Error(), "gid") {
		t.Fatalf("want gid mismatch, got %v", err)
	}
}

func TestAssertHostedPaneUID_CoversDescendantsAndPGID(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	shell, child, bg := 900001, 900002, 900003
	wantUID, wantGID := 501, 42
	stubHostedTreeSeams(t, wantUID, wantGID)
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
	if _, err := AssertHostedPaneUID("p1", wantUID); err != nil {
		t.Fatal(err)
	}
	processCredsOf = func(pid int) (processCreds, error) {
		u := wantUID
		if pid == bg {
			u = wantUID + 1
		}
		return processCreds{RUID: u, EUID: u, RGID: wantGID, EGID: wantGID}, nil
	}
	if _, err := AssertHostedPaneUID("p1", wantUID); err == nil {
		t.Fatal("background descendant wrong uid must fail")
	}
}

func TestAssertHostedPaneUID_RejectsMissingStartToken(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	stubHostedTreeSeams(t, 501, 42)
	readStartTok = func(pid int) (string, error) { return "", fmt.Errorf("empty") }
	runHerdr = func(args ...string) (string, error) {
		return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":[{"pid":900001}]}}}`, nil
	}
	if _, err := AssertHostedPaneUID("p1", 501); err == nil {
		t.Fatal("missing start token must fail lineage proof")
	}
}

func TestSignalHostedPaneTree_RevalidatesStartToken(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)
	stubHostedTreeSeams(t, 501, 42)
	fakePID := 1<<30 - 3
	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
	reads := 0
	readStartTok = func(pid int) (string, error) {
		reads++
		if reads <= 1 {
			return "tok-original", nil
		}
		return "tok-REUSED", nil
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
	if err == nil || (!strings.Contains(err.Error(), "reuse") && !strings.Contains(err.Error(), "start token")) {
		t.Fatalf("want pid-reuse refuse, got %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("must not signal on start-token reuse: %v", signals)
	}
}

func TestSignalHostedPaneTree_SignalsWhenTokenStable(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)
	stubHostedTreeSeams(t, 501, 42)
	fakePID := 1<<30 - 5
	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
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

func TestFailHostedIsolationProof_SignalsExactPIDsAndClosesTab(t *testing.T) {
	// Restored non-vacuous composition lock: FailHostedIsolationProof must
	// both signal exact PIDs and close the tab (prior reviewer finding #2).
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)
	stubHostedTreeSeams(t, 501, 42)

	fakePID := 1<<30 - 1
	closed := false
	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
	readStartTok = func(pid int) (string, error) { return "tok", nil }
	var signals []procsignal.KillCall
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
	if len(signals) < 2 {
		t.Fatalf("FailHostedIsolationProof must TERM+KILL exact PIDs, got %v", signals)
	}
	for _, c := range signals {
		if c.PID <= 1 || c.PID < 0 || c.PID == -1 {
			t.Fatalf("must never signal host-wide/sentinel target: %+v", c)
		}
		if c.PID != fakePID {
			t.Fatalf("unexpected signal target %+v want %d", c, fakePID)
		}
	}
}

func TestTerminateHostedPaneTree_NeverSignalsSelf(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)
	stubHostedTreeSeams(t, 501, 42)

	self := 777
	osGetpid = func() int { return self }
	sleepBrief = func() {}
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
	// Non-vacuous: TERM+KILL on the non-self PID only.
	if len(signals) != 2 {
		t.Fatalf("signals=%v want exactly TERM+KILL on pid 888", signals)
	}
	for _, pid := range signals {
		if pid != 888 {
			t.Fatalf("unexpected signal pid %d", pid)
		}
	}
}

func TestFailHostedIsolationProofWithLifecycle_TombsOnRollbackFailure(t *testing.T) {
	// Residual of HIGH #2: rollback failure must still Invalidate (tombstone)
	// before dropToolChild — never sticky provisional without tombstone.
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)
	stubHostedTreeSeams(t, 501, 42)

	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
	readStartTok = func(pid int) (string, error) { return "tok", nil }
	signalExact = func(pid int, sig syscall.Signal) error { return nil }

	path := filepath.Join(t.TempDir(), "lifecycle.jsonl")
	sink := &toolchild.JSONLSink{Path: path}
	lc := toolchild.NewLifecycle(toolchild.Identity{}, &toolchild.FakeTree{}, sink)
	lc.SetContext(toolchild.Identity{TabID: "t1", PaneID: "p1", Name: "worker", SessionGeneration: 1, LaunchID: "launch-1", Repository: "repo", Lane: "worker"})
	if err := lc.Provision(); err != nil {
		t.Fatal(err)
	}
	// Register lifecycle so dropToolChild is meaningful.
	toolChildMu.Lock()
	toolChildByTab["t1"] = lc
	toolChildByPane["p1"] = lc
	toolChildMu.Unlock()
	t.Cleanup(func() { dropToolChild("t1", "p1") })

	// Force rollback failure: compensate cannot find agent (list empty), so
	// rollback returns before Invalidate — cascade must still tombstone.
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":[{"pid":900001}]}}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "close" {
			return `{}`, nil
		}
		return `{}`, nil
	}

	err := FailHostedIsolationProofWithLifecycle("p1", "t1", lc, fmt.Errorf("proof failed"))
	if err == nil {
		t.Fatal("expected proof error")
	}
	if lifecycleForPane("p1") != nil || lifecycleForTab("t1") != nil {
		t.Fatal("process-local lifecycle must be dropped after cascade")
	}
	// Durable tombstone must exist.
	b, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(b), `"action":"tombstone"`) && !strings.Contains(string(b), `"action": "tombstone"`) {
		// JSON may compact differently
		if !strings.Contains(string(b), "tombstone") {
			t.Fatalf("want durable tombstone in %s, got:\n%s", path, b)
		}
	}
	if !strings.Contains(string(b), "provisional") {
		t.Fatal("test setup must have written provisional before tombstone")
	}
}

func TestAgentStartWithDecision_HostedUIDProofFailsBeforeBind(t *testing.T) {
	// Integration lock for prior HIGHs #2/#5: isolation failure before bind,
	// lifecycle tombstoned, never Bound as wrong-UID owner.
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(t.TempDir(), "launch.jsonl"))
	builderUID := os.Getuid() + 11
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", builderUID))
	t.Setenv(EnvRequireHostedUID, "1")
	pinBuilderGID(t, 42)

	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (processCreds, error)) { processCredsOf = old }(processCredsOf)
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) ([]int, error)) { listPGIDMembers = old }(listPGIDMembers)
	defer func(old func(int) ([]int, error)) { listChildPIDs = old }(listChildPIDs)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)
	defer func(old int) { agentReadyTries = old }(agentReadyTries)
	defer func(old func()) { agentReadySleep = old }(agentReadySleep)

	osGetpid = func() int { return 1 }
	sleepBrief = func() {}
	agentReadyTries = 1
	agentReadySleep = func() {}
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	readStartTok = func(pid int) (string, error) { return "tok", nil }
	// Wrong: coordinator real+effective — isolation proof must fail.
	me := os.Getuid()
	processCredsOf = func(pid int) (processCreds, error) {
		return processCreds{RUID: me, EUID: me, RGID: 42, EGID: 42}, nil
	}
	signalExact = func(pid int, sig syscall.Signal) error { return nil }

	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel,
		RequestedEffort: testWorkerEffort, TaskRef: "FAC-172", LeaseGeneration: 3,
		Scope: router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{
		Decision: d, TaskRef: "FAC-172", Repository: "repo", Lane: "worker",
		SessionGeneration: 7, LeaseGeneration: 3, Scope: router.ScopeTask,
	}

	path := filepath.Join(t.TempDir(), "lifecycle.jsonl")
	sink := &toolchild.JSONLSink{Path: path}
	lc := toolchild.NewLifecycle(toolchild.Identity{}, &toolchild.FakeTree{}, sink)
	restore := SetToolChildLifecycleFactory(func(launch.Request, string, string) (ToolChildLifecycle, error) {
		return lc, nil
	})
	defer restore()

	var started, closed bool
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "capabilities") {
			return goodCapJSON(), nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "start" {
			started = true
			return `{}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			if closed {
				return `{"error":{"code":"pane_not_found"}}`, fmt.Errorf("pane not found")
			}
			// Shell as coordinator UID — isolation proof must fail.
			return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":[{"pid":900001,"name":"zsh","argv":["zsh"]}]}}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			if closed || !started {
				return `{"result":{"agents":[]}}`, nil
			}
			return `{"result":{"agents":[{"name":"worker","agent":"codex","pane_id":"p1","tab_id":"t1"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			if closed {
				return `{"result":{"tabs":[]}}`, nil
			}
			return `{"result":{"tabs":[{"tab_id":"t1"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "close" {
			closed = true
			return `{}`, nil
		}
		return `{}`, nil
	}

	if err := PrepareToolChildLifecycle("t1", "p1", &req, "worker"); err != nil {
		t.Fatal(err)
	}
	// Kind must match decision harness (pi on current fleet).
	err = AgentStartWithDecision("worker", req.Decision.Harness, "p1", req)
	if err == nil {
		t.Fatal("expected hosted-uid proof failure")
	}
	if !strings.Contains(err.Error(), "hosted uid") && !strings.Contains(err.Error(), "BuilderUID") && !strings.Contains(err.Error(), "proof") && !strings.Contains(err.Error(), "seteuid") {
		t.Fatalf("want isolation proof failure, got %v", err)
	}
	if lc.Bound() {
		t.Fatal("must not bind wrong-UID process into tool-child inventory")
	}
	if lifecycleForPane("p1") != nil {
		t.Fatal("lifecycle must be dropped after isolation failure cleanup")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, "tombstone") {
		t.Fatalf("want durable tombstone after isolation failure, got:\n%s", body)
	}
	if !strings.Contains(body, "provisional") {
		t.Fatal("provisional must have been written at prepare")
	}
}

func TestHostedUIDIsolationRequired_ForceAndEnv(t *testing.T) {
	me := os.Getuid()
	t.Setenv(EnvBuilderUID, "")
	t.Setenv(EnvRequireHostedUID, "")
	if HostedUIDIsolationRequired() {
		t.Fatal("absent builder env must not require isolation")
	}
	// Present-but-invalid must fail closed (require path), not silently off.
	for _, bad := range []string{"5O1", "501 ", "-1", "0", "abc"} {
		t.Setenv(EnvBuilderUID, bad)
		if !HostedUIDIsolationRequired() {
			t.Fatalf("present-but-invalid %q must require isolation (fail closed)", bad)
		}
		if _, err := BuilderUID(); err == nil {
			t.Fatalf("BuilderUID must reject %q", bad)
		}
	}
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me))
	if !HostedUIDIsolationRequired() {
		t.Fatal("present builder env requires isolation even when equal coordinator")
	}
	t.Setenv(EnvBuilderUID, "")
	t.Setenv(EnvRequireHostedUID, "1")
	if !HostedUIDIsolationRequired() {
		t.Fatal("HERD_REQUIRE_HOSTED_UID=1 must force isolation path")
	}
	t.Setenv(EnvRequireHostedUID, "")
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me+1))
	if !HostedUIDIsolationRequired() {
		t.Fatal("distinct builder uid must require isolation")
	}
}

func TestExpectedBuilderGID_RejectsCoordinatorAndZero(t *testing.T) {
	// Unset branch: shell GID == coordinator fails.
	t.Setenv(EnvBuilderGID, "")
	if _, err := expectedBuilderGID(os.Getgid()); err == nil {
		t.Fatal("shell gid == coordinator must fail group-drop proof")
	}
	// Distinct shell GID succeeds when env unset.
	want := os.Getgid() + 7
	if want == 0 {
		want = 42
	}
	g, err := expectedBuilderGID(want)
	if err != nil || g != want {
		t.Fatalf("got %d %v want %d", g, err, want)
	}
	// Set branch: reject coordinator GID and non-positive.
	t.Setenv(EnvBuilderGID, fmt.Sprintf("%d", os.Getgid()))
	if _, err := expectedBuilderGID(999); err == nil {
		t.Fatal("HERD_BUILDER_GID == coordinator must fail")
	}
	t.Setenv(EnvBuilderGID, "0")
	if _, err := expectedBuilderGID(999); err == nil {
		t.Fatal("HERD_BUILDER_GID=0 must fail")
	}
	t.Setenv(EnvBuilderGID, "42")
	if os.Getgid() == 42 {
		t.Setenv(EnvBuilderGID, "43")
	}
	if g, err := expectedBuilderGID(1); err != nil || g <= 0 {
		t.Fatalf("valid builder gid: %d %v", g, err)
	}
}

func TestSignalBoundIdentity_ObserveFailureFailsClosed(t *testing.T) {
	// HIGH: read error while process still exists must not skip the kill.
	defer func(old func(int) (string, error)) { readStartTok = old }(readStartTok)
	defer func(old func(int) bool) { processExists = old }(processExists)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func() int) { osGetpid = old }(osGetpid)
	osGetpid = func() int { return 1 }
	processExists = func(int) bool { return true } // still alive
	readStartTok = func(int) (string, error) { return "", fmt.Errorf("ps: fork failed") }
	var signals int
	signalExact = func(pid int, sig syscall.Signal) error {
		signals++
		return nil
	}
	err := signalBoundIdentity(HostedProcessIdentity{PID: 900001, StartToken: "tok"}, syscall.SIGTERM)
	if err == nil || !errors.Is(err, ErrHostedUIDObserve) && !strings.Contains(err.Error(), "observation") {
		t.Fatalf("want observation failure, got %v", err)
	}
	if signals != 0 {
		t.Fatal("must not signal when observation failed")
	}
	// Gone process: idempotent success, no signal needed.
	processExists = func(int) bool { return false }
	if err := signalBoundIdentity(HostedProcessIdentity{PID: 900001, StartToken: "tok"}, syscall.SIGTERM); err != nil {
		t.Fatalf("gone process must be idempotent success: %v", err)
	}
}

func TestGetHostedPaneIdentity_SkipsGoneOptionalDescendants(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	stubHostedTreeSeams(t, 501, 42)
	listChildPIDs = func(pid int) ([]int, error) {
		if pid == 900001 {
			return []int{900099}, nil // transient child
		}
		return nil, nil
	}
	processExists = func(pid int) bool { return pid != 900099 }
	readStartTok = func(pid int) (string, error) {
		if pid == 900099 {
			return "", fmt.Errorf("no such process")
		}
		return fmt.Sprintf("tok-%d", pid), nil
	}
	runHerdr = func(args ...string) (string, error) {
		return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":[{"pid":900001}]}}}`, nil
	}
	info, err := GetHostedPaneIdentity("p1")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range info.Tree {
		if p.PID == 900099 {
			t.Fatal("gone optional descendant must be skipped, not included")
		}
	}
	// Required foreground gone → fail closed.
	processExists = func(int) bool { return false }
	readStartTok = func(int) (string, error) { return "", fmt.Errorf("no such process") }
	if _, err := GetHostedPaneIdentity("p1"); err == nil {
		t.Fatal("required shell/fg observation failure must fail closed")
	}
}

func TestSignalHostedPaneTree_GraceBetweenTermAndKill(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int, syscall.Signal) error) { signalExact = old }(signalExact)
	defer func(old func()) { sleepBrief = old }(sleepBrief)
	defer func(old func() int) { osGetpid = old }(osGetpid)
	stubHostedTreeSeams(t, 501, 42)
	fakePID := 1<<30 - 9
	osGetpid = func() int { return 1 }
	readStartTok = func(pid int) (string, error) { return "tok", nil }
	var order []string
	sleepBrief = func() { order = append(order, "sleep") }
	signalExact = func(pid int, sig syscall.Signal) error {
		order = append(order, fmt.Sprintf("%d", sig))
		return nil
	}
	runHerdr = func(args ...string) (string, error) {
		return fmt.Sprintf(`{"result":{"process_info":{"pane_id":"p1","shell_pid":%d,"foreground_processes":[{"pid":%d}]}}}`, fakePID, fakePID), nil
	}
	if err := SignalHostedPaneTree("p1"); err != nil {
		t.Fatal(err)
	}
	// Expect TERM, grace, KILL — not TERM/KILL adjacent without sleep.
	want := []string{
		fmt.Sprintf("%d", syscall.SIGTERM),
		"sleep",
		fmt.Sprintf("%d", syscall.SIGKILL),
	}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("signal order %v want %v", order, want)
	}
}

func TestNoCLISetuidEnforcementInSource(t *testing.T) {
	// Scan every package source file for CLI setuid / host-wide kill / sudo escapes.
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"runHerdrAsUID", "syscall.Setuid", "syscall.Kill(-1", "Kill(-1,",
		"\"sudo\"", "'sudo'", " doas ", "\"doas\"", "\"su\"", "launchctl asuser",
		"/usr/bin/sudo", "Credential{Uid",
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		// Allow mentions in this test file's forbidden list itself.
		if e.Name() == "hosted_uid_test.go" {
			continue
		}
		for _, bad := range forbidden {
			if strings.Contains(src, bad) {
				t.Fatalf("%s must not contain %q", e.Name(), bad)
			}
		}
	}
}

func TestMutation_ValidateCapabilityGatesRemovedWouldFail(t *testing.T) {
	good := HostedUIDCapability{
		SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true,
		ProcessLineageProof: true, TabCreateUIDFlag: "--uid",
	}
	if err := validateHostedUIDCapability(good); err != nil {
		t.Fatal(err)
	}
	mutants := []HostedUIDCapability{
		{SchemaVersion: 0, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: false, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: false, ProcessLineageProof: true, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: false, TabCreateUIDFlag: "--uid"},
		{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--user"},
	}
	for i, m := range mutants {
		if err := validateHostedUIDCapability(m); err == nil {
			t.Fatalf("mutant %d accepted", i)
		}
	}
}

func TestAssertAgentHostedAsBuilder_ExactRoutedOwner(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { readParent = old }(readParent)
	builder, gid := 501, 42
	stubHostedTreeSeams(t, builder, gid)
	argv := []string{"codex", "--model", "gpt-5.6-luna"}
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
	if err := AssertAgentHostedAsBuilder("worker", builder, gid, "codex", argv); err != nil {
		t.Fatal(err)
	}
	processCredsOf = func(pid int) (processCreds, error) {
		u := builder
		if pid == 501 {
			u = builder + 1
		}
		return processCreds{RUID: u, EUID: u, RGID: gid, EGID: gid}, nil
	}
	if err := AssertAgentHostedAsBuilder("worker", builder, gid, "codex", argv); err == nil {
		t.Fatal("wrong agent uid must fail")
	}
}


func TestAssertHostedPaneUID_RejectsSeteuidOnly(t *testing.T) {
	// HIGH: ruid remains coordinator while euid is builder — not isolation.
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	coord := os.Getuid()
	builderUID := coord + 17
	if builderUID == coord {
		t.Fatal("fixture builder uid must differ from coordinator")
	}
	pinBuilderGID(t, 42)
	oldCreds, oldTok, oldPG, oldKids, oldExists := processCredsOf, readStartTok, listPGIDMembers, listChildPIDs, processExists
	t.Cleanup(func() {
		processCredsOf, readStartTok, listPGIDMembers, listChildPIDs, processExists = oldCreds, oldTok, oldPG, oldKids, oldExists
	})
	readStartTok = func(int) (string, error) { return "tok", nil }
	listPGIDMembers = func(int) ([]int, error) { return nil, nil }
	listChildPIDs = func(int) ([]int, error) { return nil, nil }
	processExists = func(int) bool { return true }
	processCredsOf = func(int) (processCreds, error) {
		// seteuid(builder) without setuid: euid matches builder, ruid stays coordinator.
		return processCreds{RUID: coord, EUID: builderUID, RGID: 42, EGID: 42}, nil
	}
	runHerdr = func(args ...string) (string, error) {
		return `{"result":{"process_info":{"pane_id":"p1","shell_pid":900001,"foreground_processes":[{"pid":900001}]}}}`, nil
	}
	_, err := AssertHostedPaneUID("p1", builderUID)
	if err == nil {
		t.Fatal("want seteuid-only rejection")
	}
	if !strings.Contains(err.Error(), "seteuid") && !strings.Contains(err.Error(), "ruid=") {
		t.Fatalf("want ruid/euid mismatch wording, got %v", err)
	}
}

func TestAssertAgentHostedAsBuilder_PollsReadiness(t *testing.T) {
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	defer func(old func(int) (int, error)) { readParent = old }(readParent)
	builder, gid := 501, 42
	stubHostedTreeSeams(t, builder, gid)
	agentReadyTries = 3
	calls := 0
	agentReadySleep = func() { calls++ }
	readParent = func(pid int) (int, error) {
		if pid == 501 {
			return 500, nil
		}
		return 1, nil
	}
	listCalls := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			listCalls++
			if listCalls < 3 {
				return `{"result":{"agents":[]}}`, nil
			}
			return `{"result":{"agents":[{"name":"worker","agent":"codex","pane_id":"p1","tab_id":"t1"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"pane_id":"p1","shell_pid":499,"foreground_processes":[
				{"pid":500,"name":"node","argv":["node","/opt/homebrew/bin/codex"]},
				{"pid":501,"name":"codex","argv":["codex","--model","x"]}
			]}}}`, nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	if err := AssertAgentHostedAsBuilder("worker", builder, gid, "codex", []string{"codex", "--model", "x"}); err != nil {
		t.Fatal(err)
	}
	if listCalls < 3 || calls < 2 {
		t.Fatalf("expected readiness poll listCalls=%d sleepCalls=%d", listCalls, calls)
	}
}

func TestValidateHostedUIDCapability_AgentStartFlagAllowlist(t *testing.T) {
	// Restored coverage deleted in 7e156f9.
	cases := []struct {
		name string
		cap  HostedUIDCapability
		ok   bool
	}{
		{"uid", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid", AgentStartUIDFlag: "--uid"}, true},
		{"run-as-uid", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--run-as-uid"}, true},
		{"hosted-uid", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--hosted-uid", AgentStartUIDFlag: "--hosted-uid"}, true},
		{"bad-agent-flag", HostedUIDCapability{SchemaVersion: 1, HostedProcessUID: true, HostedProcessGID: true, ProcessLineageProof: true, TabCreateUIDFlag: "--uid", AgentStartUIDFlag: "--user"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHostedUIDCapability(tc.cap)
			if tc.ok && err != nil {
				t.Fatal(err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestAgentStartProcess_AttachesNegotiatedUIDFlag(t *testing.T) {
	// Wiring lock: agentStartUIDFlagArgs must reach agent start argv.
	ResetHostedUIDCapabilityCache()
	t.Cleanup(ResetHostedUIDCapabilityCache)
	me := os.Getuid()
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me+1))
	defer func(old func(args ...string) (string, error)) { runHerdr = old }(runHerdr)
	var startArgs []string
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "capabilities") {
			return goodCapJSON(), nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "start" {
			startArgs = append([]string{}, args...)
			return `{}`, nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}
	// Bypass the 500ms sleep by calling with isolation on.
	if err := agentStartProcess("worker", "pi", "p1", "--session", "/tmp/s"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(startArgs, " ")
	if !strings.Contains(joined, "--uid") || !strings.Contains(joined, fmt.Sprintf("%d", me+1)) {
		t.Fatalf("agent start missing negotiated uid flags: %v", startArgs)
	}
}


func errorsIsHostedBlocked(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "FAC-172") || strings.Contains(s, "BLOCKED") || strings.Contains(s, "hosted process UID")
}

