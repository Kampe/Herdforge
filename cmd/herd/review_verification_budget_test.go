package main

import (
	"strings"
	"testing"
)

func TestFormatNativeReviewTargetedCommandRequiresRunForHeavyPackages(t *testing.T) {
	cmd := formatNativeReviewTargetedCommand([]string{"./cmd/herd/", "./pkg/herdr/"}, []string{"TestReviewLaunchEnvPinsBudget", "TestMergeBoundedGOFLAGS"})
	if nativeReviewCommandIsFullHeavyPackage(cmd) {
		t.Fatalf("targeted command must not be a full heavy package suite: %s", cmd)
	}
	if !strings.Contains(cmd, "-run") || !strings.Contains(cmd, "TestReviewLaunchEnvPinsBudget") {
		t.Fatalf("targeted command must name tests with -run: %s", cmd)
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
