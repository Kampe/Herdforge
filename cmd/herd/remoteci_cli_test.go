package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRemoteCISettleCLIUsesExactSHAAndReadsBackPassedSettlement(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://github.com/Kampe/Herdforge.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := "merge_policy:\n  protected: true\n  required_checks: [local]\n  require_different_family_review: true\n  require_pull_request_reviews: true\n  remote_ci:\n    required: true\n    required_checks: [\"Build, Preflight & Test Suite\"]\n"
	if err := os.WriteFile(filepath.Join(repo, ".herd", "herd.yaml"), []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeDir := t.TempDir()
	fakeGH := filepath.Join(fakeDir, "gh")
	buildFake := exec.Command("go", "build", "-buildvcs=false", "-o", fakeGH, "./cmd/herd/testdata/fakegh")
	buildFake.Dir = filepath.Join("..", "..")
	if out, err := buildFake.CombinedOutput(); err != nil {
		t.Fatalf("build fake gh: %v: %s", err, out)
	}

	candidate := strings.Repeat("a", 40)
	ledger := filepath.Join(repo, ".herd", "remote-ci.jsonl")
	cmd := exec.Command(buildHerd(t),
		"remote-ci-settle",
		"--ref", "FAC-683",
		"--candidate", candidate,
		"--remote-ci-attempt", "1",
		"--remote-ci-file", ledger,
		"--timeout", "2s",
		"--poll-interval", "1ms",
		"--max-polls", "5",
	)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HERD_FAKE_GH_CANDIDATE="+candidate,
		"HERD_FAKE_GH_REPOSITORY=github.com/Kampe/Herdforge",
		"HERD_FAKE_GH_CHECK=Build, Preflight & Test Suite",
		"HERD_FAKE_GH_STATE="+filepath.Join(repo, "gh-polls"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("remote-ci-settle: %v\n%s", err, out)
	}
	var got remoteCISettleOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("machine-readable result: %v\n%s", err, out)
	}
	if got.State != "passed" || got.CandidateSHA != candidate || got.Repository != "github.com/Kampe/Herdforge" || got.Attempt != 1 || got.Polls != 2 {
		t.Fatalf("unexpected settlement result: %+v", got)
	}
	wantRemoteCIArgs := []string{"--remote-ci-attempt", "1", "--remote-ci-file", ledger}
	if !reflect.DeepEqual(got.RemoteCIArgs, wantRemoteCIArgs) {
		t.Fatalf("remote_ci_args = %q, want %q", got.RemoteCIArgs, wantRemoteCIArgs)
	}
	if strings.Contains(string(out), "merge_admit_command") || strings.Contains(string(out), "herd merge-admit") {
		t.Fatalf("output misleadingly presents an incomplete merge-admit invocation: %s", out)
	}
}
