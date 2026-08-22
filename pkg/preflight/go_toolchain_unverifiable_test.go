package preflight

import (
	"errors"
	"testing"
)

// TestUnverifiableToolchainDoesNotFailPreflight is the FAC-576 correction.
//
// A probe that cannot run tells us nothing about a mismatch, but this used to
// fail preflight outright — so a version-manager shim lacking context, a missing
// go on the child's PATH, or a sandbox all reported a toolchain conflict and
// blocked the run. That is the same error as reading "cannot prove" as "did not
// land".
func TestUnverifiableToolchainDoesNotFailPreflight(t *testing.T) {
	env := []string{"GOROOT=/some/exported/root"}
	failing := func(_ []string, _ ...string) (string, error) {
		return "", errors.New("exit status 2")
	}
	if err := checkGoToolchain(env, failing); err != nil {
		t.Fatalf("an unverifiable toolchain must not fail preflight: %v", err)
	}
}

// A real mismatch requires observing BOTH toolchains, and must still fail.
func TestObservedMismatchStillFails(t *testing.T) {
	env := []string{"GOROOT=/exported/one"}
	probe := func(_ []string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "env" {
			return "/path/resolved/two\n", nil
		}
		return "go version go1.26.6 darwin/arm64\n", nil
	}
	err := checkGoToolchain(env, probe)
	if err == nil {
		t.Fatal("two different observed toolchains must still fail")
	}
}

// Agreement passes, and an unexported GOROOT is not a question at all.
func TestAgreementAndAbsencePass(t *testing.T) {
	same := func(_ []string, _ ...string) (string, error) { return "/same/root\n", nil }
	if err := checkGoToolchain([]string{"GOROOT=/same/root"}, same); err != nil {
		t.Errorf("matching toolchains must pass: %v", err)
	}
	if err := checkGoToolchain([]string{"PATH=/usr/bin"}, same); err != nil {
		t.Errorf("no exported GOROOT is not a mismatch: %v", err)
	}
}
