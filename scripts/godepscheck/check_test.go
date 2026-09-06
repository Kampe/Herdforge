package godepscheck

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
)

func scriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(file), "..", "check-go-dependencies.zsh")
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func requireZsh(t *testing.T) string {
	t.Helper()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is required to run check-go-dependencies.zsh")
	}
	return zsh
}

func gitOK(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testgit.Command(dir, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initModuleRepo(t *testing.T, dir string) {
	t.Helper()
	gitOK(t, dir, "init", "-q", "-b", "main")
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	gitOK(t, dir, "add", "-A")
	gitOK(t, dir, "commit", "-q", "-m", msg)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installMockTrivy(t *testing.T, binDir, logPath string, exitCode int) {
	t.Helper()
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Last argument is the module directory; record it one path per line so
	// spaces and unusual names survive. Flag presence is checked separately.
	body := fmt.Sprintf("#!/bin/sh\n"+
		"log=%s\n"+
		"code=%s\n"+
		"eval \"target=\\${$#}\"\n"+
		"printf '%%s\\n' \"$target\" >> \"$log\"\n"+
		"for arg in \"$@\"; do\n"+
		"printf '%%s\\0' \"$arg\" >> \"$log.args\"\n"+
		"done\n"+
		"printf '\\n' >> \"$log.args\"\n"+
		"exit \"$code\"\n", shellSingleQuote(logPath), strconv.Itoa(exitCode))
	if err := os.WriteFile(filepath.Join(binDir, "trivy"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func scriptEnv(dir, mockBin string) []string {
	base := testgit.Command(dir, "status").Env
	out := make([]string, 0, len(base)+2)
	pathSet := false
	for _, item := range base {
		key, _, _ := strings.Cut(item, "=")
		if key == "PATH" {
			out = append(out, "PATH="+mockBin+string(os.PathListSeparator)+os.Getenv("PATH"))
			pathSet = true
			continue
		}
		out = append(out, item)
	}
	if !pathSet {
		out = append(out, "PATH="+mockBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return out
}

func runDepsScript(t *testing.T, dir, mockBin string, extraEnv []string) (stdout, stderr string, err error) {
	t.Helper()
	zsh := requireZsh(t)
	cmd := exec.Command(zsh, "-f", scriptPath(t))
	cmd.Dir = dir
	cmd.Env = append(scriptEnv(dir, mockBin), extraEnv...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

func TestScriptRejectsGitLsFilesPipeline(t *testing.T) {
	body, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("while IFS= read -r -d ''")) {
		t.Fatal("production script still uses git ls-files -z | while-read pipeline")
	}
	if !bytes.Contains(body, []byte(`${(0)"$(git ls-files -z)"}`)) {
		t.Fatal("production script must NUL-split git ls-files -z like security-gate.zsh")
	}
}

func TestTrackedModuleEnumeration(t *testing.T) {
	requireZsh(t)
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		want    []string
		wantErr string
	}{
		{
			name: "root_and_nested_sorted",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/root\n")
				writeFile(t, filepath.Join(dir, "pkg/zebra/go.mod"), "module example.invalid/zebra\n")
				writeFile(t, filepath.Join(dir, "pkg/alpha/go.mod"), "module example.invalid/alpha\n")
				commitAll(t, dir, "modules")
			},
			want: []string{".", "pkg/alpha", "pkg/zebra"},
		},
		{
			name: "spaces_and_unusual_names",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/root\n")
				writeFile(t, filepath.Join(dir, "mod with space/go.mod"), "module example.invalid/space\n")
				writeFile(t, filepath.Join(dir, "mod (parens)/go.mod"), "module example.invalid/parens\n")
				writeFile(t, filepath.Join(dir, "mod'quote/go.mod"), "module example.invalid/quote\n")
				commitAll(t, dir, "unusual names")
			},
			want: []string{".", "mod (parens)", "mod with space", "mod'quote"},
		},
		{
			name: "excludes_worktrees_and_herd",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/root\n")
				writeFile(t, filepath.Join(dir, ".worktrees/live/go.mod"), "module example.invalid/wt\n")
				writeFile(t, filepath.Join(dir, ".herd/worktrees/x/go.mod"), "module example.invalid/herd\n")
				writeFile(t, filepath.Join(dir, "nested/.worktrees/x/go.mod"), "module example.invalid/nestedwt\n")
				commitAll(t, dir, "with excluded trees")
			},
			want: []string{"."},
		},
		{
			name: "no_modules_fail_closed",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "README"), "no modules\n")
				commitAll(t, dir, "empty of go.mod")
			},
			wantErr: "no tracked Go modules found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			initModuleRepo(t, dir)
			tt.setup(t, dir)

			mockBin := filepath.Join(dir, "mock-bin")
			logPath := filepath.Join(dir, "trivy.log")
			installMockTrivy(t, mockBin, logPath, 0)

			stdout, stderr, err := runDepsScript(t, dir, mockBin, nil)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got success stdout=%q stderr=%q", tt.wantErr, stdout, stderr)
				}
				if !strings.Contains(stderr, tt.wantErr) {
					t.Fatalf("stderr=%q want substring %q", stderr, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("script failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
			}
			got := readLines(t, logPath)
			want := append([]string(nil), tt.want...)
			sort.Strings(want)
			gotSorted := append([]string(nil), got...)
			sort.Strings(gotSorted)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Fatalf("scanned modules in order = %q, want LC_ALL=C sorted %q (sorted-copy equal=%v)", got, tt.want, strings.Join(gotSorted, "\n") == strings.Join(want, "\n"))
			}
			argvLog, _ := os.ReadFile(logPath + ".args")
			if !bytes.Contains(argvLog, []byte("--skip-dirs\x00.worktrees")) || !bytes.Contains(argvLog, []byte("--skip-dirs\x00.herd")) {
				t.Fatalf("trivy argv missing skip-dirs for .worktrees/.herd: %q", argvLog)
			}
		})
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	return strings.Split(string(body), "\n")
}

func TestTrivyNonzeroFailsClosed(t *testing.T) {
	requireZsh(t)
	dir := t.TempDir()
	initModuleRepo(t, dir)
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/root\n")
	commitAll(t, dir, "one module")
	mockBin := filepath.Join(dir, "mock-bin")
	installMockTrivy(t, mockBin, filepath.Join(dir, "trivy.log"), 1)
	_, stderr, err := runDepsScript(t, dir, mockBin, nil)
	if err == nil {
		t.Fatal("expected nonzero exit when trivy exits 1")
	}
	_ = stderr
}

func TestMissingTrivyFailsClosed(t *testing.T) {
	requireZsh(t)
	dir := t.TempDir()
	initModuleRepo(t, dir)
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/root\n")
	commitAll(t, dir, "one module")
	emptyBin := filepath.Join(dir, "empty-bin")
	if err := os.MkdirAll(emptyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// Keep git visible without inheriting a host directory that also contains trivy.
	shim := "#!/bin/sh\nexec " + shellSingleQuote(gitPath) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(emptyBin, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	zsh := requireZsh(t)
	cmd := exec.Command(zsh, "-f", scriptPath(t))
	cmd.Dir = dir
	env := scriptEnv(dir, emptyBin)
	for i, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			env[i] = "PATH=" + emptyBin
		}
	}
	cmd.Env = env
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err == nil {
		t.Fatal("expected error when trivy is missing")
	}
	if !strings.Contains(errBuf.String(), "trivy is required") {
		t.Fatalf("stderr=%q want trivy is required", errBuf.String())
	}
}

func TestNoninteractiveInvocationCompletes(t *testing.T) {
	requireZsh(t)
	dir := t.TempDir()
	initModuleRepo(t, dir)
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/root\n")
	commitAll(t, dir, "one module")
	mockBin := filepath.Join(dir, "mock-bin")
	installMockTrivy(t, mockBin, filepath.Join(dir, "trivy.log"), 0)
	stdout, stderr, err := runDepsScript(t, dir, mockBin, []string{"TERM=dumb"})
	if err != nil {
		t.Fatalf("noninteractive run failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "Scanning tracked Go modules") {
		t.Fatalf("stdout missing scan banner: %q", stdout)
	}
}
