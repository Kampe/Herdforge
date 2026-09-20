package security

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
)

// The oracle counts actual scanner invocations, not the gate's progress text
// or a second implementation of its module discovery. The mutant executes the
// same copied production script successfully before this oracle rejects it.
func TestSecurityGateModulesExactlyOnce(t *testing.T) {
	f := newSecurityModuleFixture(t)
	pristine := append([]byte(nil), f.script...)
	baseline := f.run(t, "baseline", "", "")
	if err := securityModuleOrder(baseline); err != nil {
		t.Fatal(err)
	}
	f.retain(t, "baseline", "oracle.txt", []byte("PASS root then nested exactly once\n"))

	const unique = "\ttypeset -aU gosec_modules\n"
	if bytes.Count(pristine, []byte(unique)) != 1 {
		t.Fatal("duplicate-enumeration mutation anchor must occur exactly once")
	}
	mutant := bytes.Replace(pristine, []byte(unique), []byte("\ttypeset -a gosec_modules\n"), 1)
	f.write(t, "scripts/security-gate.zsh", mutant, 0o755)
	t.Cleanup(func() {
		f.write(t, "scripts/security-gate.zsh", pristine, 0o755)
		got, err := os.ReadFile(filepath.Join(f.repo, "scripts/security-gate.zsh"))
		if err != nil || !bytes.Equal(got, pristine) {
			t.Errorf("cleanup did not restore pristine script bytes: %v", err)
		}
		f.retain(t, "cleanup", "script.zsh", got)
		if !t.Failed() {
			f.retain(t, "cleanup", "complete.txt", []byte("PASS all module controls and exact script restoration\n"))
		}
	})
	f.checkSyntax(t)
	mutantCalls := f.run(t, "duplicate-enumeration", "", "")
	const causal = "root module scanned 2 times; want exactly once"
	if err := securityModuleOrder(mutantCalls); err == nil || err.Error() != causal {
		t.Fatalf("WRONG-ASSERTION: duplicate enumeration must fail %q; got %v", causal, err)
	}
	// The nested module must still be present: a missing module is not a kill
	// for the duplicate-root defect.
	if strings.Count(strings.Join(mutantCalls, "\n"), "example.invalid/nested") != 1 {
		t.Fatalf("mutant lost nested module coverage: %q", mutantCalls)
	}
	t.Logf("CAUSAL-FAIL: %s; calls=%q; gate exit=0", causal, mutantCalls)
	f.retain(t, "duplicate-enumeration", "oracle.txt", []byte("CAUSAL-FAIL: "+causal+"; nested present exactly once; gate exit=0\n"))

	f.write(t, "scripts/security-gate.zsh", pristine, 0o755)
	restored, err := os.ReadFile(filepath.Join(f.repo, "scripts/security-gate.zsh"))
	if err != nil || !bytes.Equal(pristine, restored) {
		t.Fatalf("restored script bytes differ: %v", err)
	}
	if err := securityModuleOrder(f.run(t, "restored", "", "")); err != nil {
		t.Fatal(err)
	}
	f.retain(t, "restored", "oracle.txt", []byte("PASS root then nested exactly once; pristine bytes restored\n"))

	// A deduplication repair must not become a root-only scan or erase the
	// existing fail-closed scanner/report/baseline behavior. All modes run the
	// actual production gate; only the external scanner response is controlled.
	for _, tc := range []struct{ mode, want string }{
		{"exit", "gosec scanner failed with exit status 2"},
		{"empty", "gosec produced no complete single-document JSON report"},
		{"malformed", "gosec produced no complete single-document JSON report"},
		{"multiple", "gosec produced no complete single-document JSON report"},
		{"error", "gosec produced no complete single-document JSON report"},
		{"high", "unreviewed HIGH finding: G104 nested module/fixture.go:3"},
		{"timeout", "gosec timed out after 1s"},
		{"gitleaks-exit", "gitleaks scanner failed with exit status 2"},
		{"gitleaks-malformed", "gitleaks produced no complete JSON null/array report"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			calls := f.run(t, tc.mode, tc.mode, tc.want)
			if err := securityModuleOrder(calls); err != nil {
				t.Fatal(err)
			}
		})
	}

	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte("G104|nested module/fixture.go|3")))
	for _, tc := range []struct{ name, mode, expiry, refusal string }{
		{"exact baseline", "high", "2999-12-31", ""},
		{"stale baseline", "", "2999-12-31", "stale baseline entry " + fingerprint},
		{"expired baseline", "high", "2000-01-01", "expired baseline entry " + fingerprint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := fmt.Sprintf("G104\tnested module/fixture.go\t3\t%s\tfixture finding\towner\t%s\n", fingerprint, tc.expiry)
			f.write(t, "security/baselines/gosec-high.tsv", []byte(row), 0o644)
			if err := securityModuleOrder(f.run(t, tc.name, tc.mode, tc.refusal)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func securityModuleOrder(calls []string) error {
	rootCount := 0
	for _, call := range calls {
		if call == "example.invalid/root" {
			rootCount++
		}
	}
	if rootCount != 1 {
		return fmt.Errorf("root module scanned %d times; want exactly once", rootCount)
	}
	if len(calls) != 2 || calls[0] != "example.invalid/root" || calls[1] != "example.invalid/nested" {
		return fmt.Errorf("module invocation order %q; want root then nested exactly once", calls)
	}
	return nil
}

type securityModuleFixture struct {
	repo, bin, trace, evidence string
	script                     []byte
	env                        []string
}

func newSecurityModuleFixture(t *testing.T) *securityModuleFixture {
	t.Helper()
	for _, tool := range []string{"zsh", "jq", "shasum", "pkill"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "security-gate.zsh"))
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	f := &securityModuleFixture{repo: filepath.Join(parent, "repo with spaces"), bin: filepath.Join(parent, "bin"), trace: filepath.Join(parent, "calls"), script: script, evidence: os.Getenv("FAC838_EVIDENCE_DIR")}
	for _, dir := range []string{f.repo, f.bin, filepath.Join(parent, "zsh-config")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.write(t, "scripts/security-gate.zsh", script, 0o755)
	for path, body := range map[string]string{
		"go.mod":                            "module example.invalid/root\n",
		"fixture.go":                        "package fixture\n",
		"nested module/go.mod":              "module example.invalid/nested\n",
		"nested module/fixture.go":          "package fixture\n",
		"security/baselines/gosec-high.tsv": "# empty reviewed baseline\n",
		"security/baselines/gitleaks.tsv":   "# empty reviewed baseline\n",
	} {
		f.write(t, path, []byte(body), 0o644)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "tracked module fixture"}} {
		if out, err := testgit.Command(f.repo, args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture git %v: %v\n%s", args, err, out)
		}
	}
	// These files were never added. A filesystem-wide module scan would invoke
	// the recorder for them and fail the same exact module inventory oracle.
	f.write(t, "untracked/go.mod", []byte("module example.invalid/untracked\n"), 0o644)
	f.write(t, ".herd/decoy/go.mod", []byte("module example.invalid/runtime\n"), 0o644)
	for name, body := range map[string]string{"gosec": securityModuleGosec, "gitleaks": securityModuleGitleaks} {
		if err := os.WriteFile(filepath.Join(f.bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range testgit.Command(f.repo).Env {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "PATH", "GOSEC_TIMEOUT", "GITLEAKS_TIMEOUT", "SECURITY_GATE_TIMEOUT", "GITLEAKS_BASELINE", "ZDOTDIR", "LC_ALL", "FAC838_TRACE", "FAC838_MODE":
			continue
		}
		f.env = append(f.env, item)
	}
	f.env = append(f.env, "PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GOSEC_TIMEOUT=10", "GITLEAKS_TIMEOUT=10", "ZDOTDIR="+filepath.Join(parent, "zsh-config"), "LC_ALL=C", "FAC838_TRACE="+f.trace)
	return f
}

func (f *securityModuleFixture) write(t *testing.T, name string, body []byte, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(f.repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
}

// Only the focused hosted invocation sets evidence. Ordinary package tests keep
// their disposable fixtures in TempDir. Exclusive files reject evidence reuse.
func (f *securityModuleFixture) retain(t *testing.T, phase, name string, body []byte) {
	t.Helper()
	if f.evidence == "" {
		return
	}
	dir := filepath.Join(f.evidence, strings.ReplaceAll(phase, " ", "-"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(body)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("retain %s/%s: write=%v close=%v", phase, name, writeErr, closeErr)
	}
}

func (f *securityModuleFixture) checkSyntax(t *testing.T) {
	t.Helper()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skipf("zsh unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, zsh, "-n", "scripts/security-gate.zsh")
	cmd.Dir, cmd.Env = f.repo, f.env
	out, err := cmd.CombinedOutput()
	f.retain(t, "duplicate-enumeration", "syntax.log", out)
	if err != nil {
		t.Fatalf("mutation must remain valid shell: %v\n%s", err, out)
	}
}

func (f *securityModuleFixture) run(t *testing.T, phase, mode, refusal string) []string {
	t.Helper()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skipf("zsh unavailable: %v", err)
	}
	if err := os.WriteFile(f.trace, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, zsh, "-f", "scripts/security-gate.zsh")
	cmd.Dir = f.repo
	cmd.Env = append(append([]string(nil), f.env...), "FAC838_MODE="+mode)
	if mode == "timeout" {
		cmd.Env = append(cmd.Env, "GOSEC_TIMEOUT=1")
	}
	cmd.WaitDelay = time.Second
	out, runErr := cmd.CombinedOutput()
	// Preserve raw output and the exact executed script before assertions, so
	// setup, timeout and unexpected failures remain inspectable after TempDir.
	f.retain(t, phase, "gate.log", out)
	trace, err := os.ReadFile(f.trace)
	if err != nil {
		t.Fatal(err)
	}
	f.retain(t, phase, "scanner-calls.txt", trace)
	script, err := os.ReadFile(filepath.Join(f.repo, "scripts/security-gate.zsh"))
	if err != nil {
		t.Fatal(err)
	}
	f.retain(t, phase, "script.zsh", script)
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	f.retain(t, phase, "result.txt", []byte(fmt.Sprintf("exit=%d\nouter-timeout=%t\nexpected-refusal=%s\n", exitCode, ctx.Err() != nil, refusal)))
	if ctx.Err() != nil {
		t.Fatalf("%s exceeded outer execution bound: %v\n%s", phase, ctx.Err(), out)
	}
	if refusal == "" && runErr != nil {
		t.Fatalf("%s gate must succeed before module oracle: %v\n%s", phase, runErr, out)
	}
	if refusal != "" && (runErr == nil || !strings.Contains(string(out), refusal)) {
		t.Fatalf("%s must refuse with %q: %v\n%s", phase, refusal, runErr, out)
	}
	calls := strings.Fields(string(trace))
	t.Logf("%s calls=%q gate-error=%v\n%s", phase, calls, runErr, out)
	return calls
}

const securityModuleGosec = `#!/usr/bin/env zsh
set -euo pipefail
IFS=' ' read -r keyword module < go.mod
print -r -- "$module" >> "$FAC838_TRACE"
out=""
for arg in "$@"; do
  [[ "$arg" != -out=* ]] || out="${arg#-out=}"
done
[[ -n "$out" ]] || exit 91
if [[ "$module" == example.invalid/nested ]]; then
  case "${FAC838_MODE-}" in
    exit) print '{"Issues":null}' > "$out"; exit 2 ;;
    empty) : > "$out"; exit 0 ;;
    malformed) print '{' > "$out"; exit 0 ;;
    multiple) print '{"Issues":null}\n{"Issues":null}' > "$out"; exit 0 ;;
    error) print '{"Issues":null,"error":"unavailable"}' > "$out"; exit 0 ;;
    high) jq -n --arg file "$PWD/fixture.go" '{Issues:[{severity:"HIGH",rule_id:"G104",file:$file,line:"3"}]}' > "$out"; exit 0 ;;
    timeout) exec sleep 30 ;;
  esac
fi
print '{"Issues":null}' > "$out"
`

const securityModuleGitleaks = `#!/usr/bin/env zsh
set -euo pipefail
for ((i=1; i<=$#; i++)); do
  if [[ "${@[i]}" == --report-path ]]; then
    case "${FAC838_MODE-}" in
      gitleaks-exit) print '[]' > "${@[i+1]}"; exit 2 ;;
      gitleaks-malformed) print '{' > "${@[i+1]}"; exit 0 ;;
    esac
    print '[]' > "${@[i+1]}"
    exit 0
  fi
done
exit 91
`
