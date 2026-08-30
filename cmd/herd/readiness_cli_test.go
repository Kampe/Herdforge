package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-636: MergeReadiness existed only as a Go API, so the coordinator's only
// option was grepping the ledger -- which is how verdict ROWS get counted instead
// of verdict VALUES read. That mistake sent eight FAILED PRs to the review
// supervisor as "ready to merge", twice. A safety rule with no callable surface is
// a rule nobody follows.
func TestReadinessCLI_ExitsNonZeroWhenAnyCandidateIsBlocked(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herd")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOROOT=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("build unavailable: %v (%s)", err, out)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	pass := strings.Repeat("a", 40)
	fail := strings.Repeat("b", 40)
	rows := `{"event":"record","sha":"` + pass + `","reviewer":"r1","builder_family":"anthropic","reviewer_family":"xai","tier":"R3"}` + "\n" +
		`{"event":"verdict","sha":"` + pass + `","reviewer":"r1","verdict":"PASS","builder_family":"anthropic","reviewer_family":"xai","verification_digest":"digest"}` + "\n" +
		`{"event":"verdict","sha":"` + fail + `","reviewer":"r2","verdict":"FAIL","builder_family":"xai"}` + "\n"
	if err := os.WriteFile(filepath.Join(root, ".herd", "review-ledger.jsonl"), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (string, int) {
		cmd := exec.Command(bin, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HERD_ROOT="+root)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}

	// A clean provable PASS is ready and exits 0.
	out, code := run("review-ledger", "readiness", pass)
	if code != 0 || !strings.Contains(out, `"ready":true`) {
		t.Fatalf("clean PASS must be ready with exit 0: code=%d out=%s", code, out)
	}
	// A FAIL is not ready and MUST exit non-zero, so a shell caller cannot read
	// "some blocked" as "all clear".
	out, code = run("review-ledger", "readiness", fail)
	if code == 0 {
		t.Fatalf("a FAIL candidate must exit non-zero: out=%s", out)
	}
	if strings.Contains(out, `"ready":true`) {
		t.Fatalf("a FAIL must never report ready: %s", out)
	}
	// Mixed batch: one ready, one blocked -> non-zero overall.
	_, code = run("review-ledger", "readiness", pass, fail)
	if code == 0 {
		t.Fatal("a batch containing a blocked candidate must exit non-zero")
	}
}
