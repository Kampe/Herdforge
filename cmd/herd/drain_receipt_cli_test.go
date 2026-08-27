package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/drainreceipt"
)

func initDrainRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "test@invalid")
	git("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "README")
	git("commit", "-m", "init")
	return root
}

// FAC-605: a bounded drain through runDrainCommand must leave a durable
// receipt. Absence of a receipt after the command returns is the defect.
// This drives the operator path (runDrainCommand), not a helper beside it.
func TestDrainCommandWritesDurableReceipt(t *testing.T) {
	root := initDrainRepo(t)

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	t.Setenv("HERD_DRAIN_TIMEOUT", "1ns")
	t.Setenv("HERD_DRAIN_REVIEW_TIMEOUT", "1ns")

	var out, errOut bytes.Buffer
	_ = runDrainCommand([]string{"--json", "--quiet"}, &out, &errOut)

	r, loadErr := drainreceipt.Load(root)
	if loadErr != nil {
		t.Fatalf("drain left no durable receipt: %v (stderr=%s)", loadErr, errOut.String())
	}
	if r.Status != drainreceipt.StatusTimeout && r.Status != drainreceipt.StatusCompleted {
		t.Fatalf("status=%q want timeout|completed", r.Status)
	}
	if r.Bound == "" {
		t.Fatal("receipt must record the bound")
	}
	if r.Status == drainreceipt.StatusRunning {
		t.Fatal("running is not a terminal receipt after the command returns")
	}
}

// FAC-605: a prior timeout receipt's resume_cursor is consumed on the next
// drain (logged before Begin overwrites the receipt).
func TestDrainCommandResumesFromPriorTimeoutCursor(t *testing.T) {
	root := initDrainRepo(t)
	if err := drainreceipt.MarkTimeout(root, "review-scan", "abc123deadbeef", 12, 100); err != nil {
		// MarkTimeout without Begin still writes a timeout receipt.
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	t.Setenv("HERD_DRAIN_TIMEOUT", "1ns")
	t.Setenv("HERD_DRAIN_REVIEW_TIMEOUT", "1ns")

	var out, errOut bytes.Buffer
	_ = runDrainCommand([]string{"--json", "--quiet"}, &out, &errOut)
	if !bytes.Contains(errOut.Bytes(), []byte("resume_cursor=abc123deadbeef")) {
		t.Fatalf("expected resume log for prior cursor; stderr=%s", errOut.String())
	}
	r, loadErr := drainreceipt.Load(root)
	if loadErr != nil {
		t.Fatalf("second drain left no receipt: %v", loadErr)
	}
	if r.Status == drainreceipt.StatusRunning {
		t.Fatal("second drain must terminate the receipt")
	}
}
