package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatNativeReviewTargetedCommandRequiresRunForHeavyPackages(t *testing.T) {
	cmd := formatNativeReviewTargetedCommand([]string{"./cmd/herd/", "./pkg/herdr/"}, []string{"TestReviewLaunchEnvPinsBudget", "TestMergeBoundedGOFLAGS"})
	if nativeReviewCommandIsFullHeavyPackage(cmd) {
		t.Fatalf("targeted command must not be a full heavy package suite: %s", cmd)
	}
	want := nativeReviewRunFlag([]string{"TestReviewLaunchEnvPinsBudget", "TestMergeBoundedGOFLAGS"})
	if !strings.Contains(cmd, want) {
		t.Fatalf("targeted command must quote anchored -run regex %s, got %s", want, cmd)
	}
	if strings.Contains(cmd, "-run TestReviewLaunchEnvPinsBudget|TestMergeBoundedGOFLAGS") {
		t.Fatalf("unquoted pipe -run is a shell pipeline: %s", cmd)
	}
	if !strings.Contains(cmd, "-timeout="+nativeReviewTargetTimeout) {
		t.Fatalf("targeted command must bound timeout: %s", cmd)
	}
}

func TestFormatNativeReviewTargetedCommandWithoutNamesDoesNotCallFullHerdPackage(t *testing.T) {
	cmd := formatNativeReviewTargetedCommand([]string{"./cmd/herd/", "./pkg/herdr/"}, nil)
	if nativeReviewCommandIsFullHeavyPackage(cmd) {
		t.Fatalf("missing names must not emit full-package go test: %s", cmd)
	}
	if strings.Contains(cmd, "go test -count=1 ./cmd/herd/") || strings.Contains(cmd, "go test -count=1 ./pkg/herdr/") {
		t.Fatalf("FAC-852 packet gap: full package listed as targeted: %s", cmd)
	}
	if !strings.Contains(cmd, "hosted CI") {
		t.Fatalf("must defer full packages to hosted CI: %s", cmd)
	}
}

func TestFormatNativeReviewTargetedCommandLightPackagesStayScoped(t *testing.T) {
	cmd := formatNativeReviewTargetedCommand([]string{"./pkg/preflight/"}, nil)
	if !strings.Contains(cmd, "./pkg/preflight/") {
		t.Fatalf("light package should remain in the command: %s", cmd)
	}
	if nativeReviewCommandIsFullHeavyPackage(cmd) {
		t.Fatalf("light package command treated as heavy: %s", cmd)
	}
}

func TestReviewVerificationBudgetForbidsFullHerdPackageAsTargetedFirst(t *testing.T) {
	section := reviewVerificationBudgetSection(formatNativeReviewTargetedCommand([]string{"./cmd/herd/"}, []string{"TestPrepareStandingWorktree"}))
	if nativeReviewCommandIsFullHeavyPackage(section) && !strings.Contains(section, "-run") {
		t.Fatal("budget section must not instruct full-package herd tests as targeted-first")
	}
	if !strings.Contains(section, "Build, Preflight & Test Suite") || !strings.Contains(section, "verification.test_command") {
		t.Fatal("budget section must name hosted official gate reuse")
	}
	if !strings.Contains(section, "-run") {
		t.Fatal("budget section targeted line must keep -run")
	}
}

func TestNativeReviewTestFuncNames(t *testing.T) {
	src := "package herdr\n\nfunc TestMergeBoundedGOFLAGS(t *testing.T) {}\nfunc helper() {}\nfunc TestReviewLaunchEnvPinsBudget(t *testing.T) {}\n"
	got := nativeReviewTestFuncNames(src)
	if len(got) != 2 || got[0] != "TestMergeBoundedGOFLAGS" || got[1] != "TestReviewLaunchEnvPinsBudget" {
		t.Fatalf("names=%v", got)
	}
}

func TestQuotedRunRegexSurvivesDisposableShellWithTwoRealTests(t *testing.T) {
	dir := t.TempDir()
	mod := `module herd.example/runquote

go 1.22
`
	src := `package runquote

import "testing"

func TestAlpha(t *testing.T) {}
func TestBeta(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quote_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	quoted := "go test -count=1 -p=1 -timeout=" + nativeReviewTargetTimeout + " " + nativeReviewRunFlag([]string{"TestAlpha", "TestBeta"}) + " ."
	unquoted := "go test -count=1 -p=1 -timeout=" + nativeReviewTargetTimeout + " -run TestAlpha|TestBeta ."

	unquotedRun := exec.Command("sh", "-c", unquoted)
	unquotedRun.Dir = dir
	unquotedOut, unquotedErr := unquotedRun.CombinedOutput()
	if unquotedErr == nil {
		t.Fatalf("unquoted pipe must be a shell pipeline, got success:\n%s", unquotedOut)
	}
	if !strings.Contains(string(unquotedOut), "TestBeta") {
		t.Fatalf("unquoted pipeline must try to execute the second Test name, got %v\n%s", unquotedErr, unquotedOut)
	}

	quotedRun := exec.Command("sh", "-c", quoted)
	quotedRun.Dir = dir
	quotedRun.Env = append(os.Environ(), "GOMAXPROCS=2")
	quotedOut, quotedErr := quotedRun.CombinedOutput()
	if quotedErr != nil {
		t.Fatalf("quoted anchored -run must run both tests: %v\ncmd=%s\n%s", quotedErr, quoted, quotedOut)
	}
	text := string(quotedOut)
	if !strings.Contains(text, "ok") {
		t.Fatalf("quoted command produced no ok: %s", text)
	}
}

func TestDrainReviewPacketDoesNotCallFullCmdHerdTargeted(t *testing.T) {
	body := drainReviewPacket("FAC-852", strings.Repeat("a", 40), t.TempDir(), "review-supervisor")
	if strings.Contains(body, "go test -count=1 ./cmd/herd/") || strings.Contains(body, "go test -count=1 ./pkg/herdr/") {
		t.Fatalf("drain packet still lists full heavy packages as targeted:\n%s", body)
	}
	if strings.Contains(body, "2. go test ./...") || strings.Contains(body, "Targeted tests: go test ./...") {
		t.Fatalf("drain packet must not fall back to go test ./... as targeted-first:\n%s", body)
	}
	if !strings.Contains(body, "Build, Preflight & Test Suite") {
		t.Fatal("drain packet must cite hosted collector")
	}
}
