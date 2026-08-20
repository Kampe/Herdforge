package buildherd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildHerdAtomicInstall(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skipf("zsh is not installed: %v", err)
	}
	repo := t.TempDir()
	if out, err := run(repo, "git", "init", "-b", "main"); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	run(repo, "git", "config", "user.email", "test@example.com")
	run(repo, "git", "config", "user.name", "test")
	write(t, filepath.Join(repo, "go.mod"), "module example.com/test\n\ngo 1.25\n")
	run(repo, "git", "add", "go.mod")
	if out, err := run(repo, "git", "commit", "-m", "fixture"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	old := []byte("old executable")
	write(t, filepath.Join(repo, "bin", "herd"), string(old))
	if err := os.Symlink("bin/herd", filepath.Join(repo, "herd")); err != nil {
		t.Fatal(err)
	}
	fakeGo := filepath.Join(t.TempDir(), "go")

	cases := []struct {
		name string
		fail bool
		want string
	}{
		{name: "failed build preserves prior binary", fail: true, want: string(old)},
		{name: "successful build replaces atomically", want: "revision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			revBytes, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Fatal(err)
			}
			fakeSource := `#!/bin/sh
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then shift; out=$1; fi
  shift
done
printf '#!/bin/sh\necho revision %s\n' "$HERD_EXPECTED_REV" > "$out"
chmod +x "$out"
`
			if tc.fail {
				fakeSource = "#!/bin/sh\nexit 7\n"
			}
			write(t, fakeGo, fakeSource)
			if err := os.Chmod(fakeGo, 0o755); err != nil {
				t.Fatal(err)
			}
			env := append(os.Environ(), "HERD_GO="+fakeGo, "HERD_EXPECTED_REV="+strings.TrimSpace(string(revBytes)))
			cmd := exec.Command("zsh", filepath.Join(repoRoot(t), "scripts", "build-herd.zsh"), repo)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if tc.fail && err == nil {
				t.Fatalf("failed build succeeded: %s", out)
			}
			if !tc.fail && err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			got, readErr := os.ReadFile(filepath.Join(repo, "bin", "herd"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(got), tc.want) {
				t.Fatalf("installed binary = %q, want %q", got, tc.want)
			}
		})
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(root))
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
