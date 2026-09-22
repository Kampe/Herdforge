package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

func TestArtifactArchiveCLIDisposableProof(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".herd/receipts/\nbin/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "base")
	wt := filepath.Join(root, ".herd", "worktrees", "fac-843")
	git("worktree", "add", "-q", wt, "HEAD")
	if err := os.MkdirAll(filepath.Join(wt, ".herd", "receipts"), 0o755); err != nil {
		t.Fatal(err)
	}
	receipt := []byte(`{"ref":"FAC-843","proof":true}`)
	if err := os.WriteFile(filepath.Join(wt, ".herd", "receipts", "FAC-843.json"), receipt, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)
	t.Cleanup(worktree.InstallTestSafetyProbes(nil, nil))
	if err := runArtifactArchiveArgs([]string{"--target", wt, "--json"}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".herd", "receipts", "FAC-843.json")); err != nil {
		t.Fatal("dry-run removed the receipt")
	}
	if err := runArtifactArchiveArgs([]string{"--target", wt, "--act", "--json"}); err != nil {
		t.Fatalf("act: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".herd", "receipts", "FAC-843.json")); !os.IsNotExist(err) {
		t.Fatal("act must remove the worktree receipt after archive readback")
	}
	manDir := filepath.Join(root, ".herd", "artifact-archive", "manifests")
	ents, err := os.ReadDir(manDir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("manifests: %v %v", err, ents)
	}
	raw, err := os.ReadFile(filepath.Join(manDir, ents[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var man worktree.ArtifactManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatal(err)
	}
	if len(man.Entries) == 0 || man.Entries[0].Kind != worktree.ArtifactReceipt {
		t.Fatalf("receipt must lead the manifest: %+v", man.Entries)
	}
	obj := filepath.Join(root, ".herd", "artifact-archive", "objects", "sha256", man.Entries[0].Digest[:2], man.Entries[0].Digest)
	got, err := os.ReadFile(obj)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(receipt) {
		t.Fatalf("archived receipt bytes %q want %q", got, receipt)
	}
}
