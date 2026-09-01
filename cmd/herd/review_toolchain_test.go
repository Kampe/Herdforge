package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeReviewTool(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func capableReviewGit(t *testing.T, dir string) string {
	t.Helper()
	return writeReviewTool(t, dir, "git", `
if [ "$1" = "--version" ]; then
  printf 'git version 2.53.0\n'
  exit 0
fi
case " $* " in
  *" merge-tree --write-tree --merge-base=HEAD HEAD HEAD "*)
    printf '0123456789012345678901234567890123456789\n'
    exit 0
    ;;
esac
printf 'unexpected git argv: %s\n' "$*" >&2
exit 91
`)
}

func capableReviewGo(t *testing.T, dir string) string {
	t.Helper()
	return writeReviewTool(t, dir, "go", `
if [ "$1" = "version" ]; then
  printf 'go version go1.25.1 test/arch\n'
  exit 0
fi
if [ "$1" = "env" ] && [ "$2" = "GOROOT" ] && [ "$3" = "GOTOOLDIR" ]; then
  printf '/toolchains/go1.25.1\n/toolchains/go1.25.1/pkg/tool/test_arch\n'
  exit 0
fi
printf 'unexpected go argv: %s\n' "$*" >&2
exit 92
`)
}

func TestReviewToolchainPreflightRefusesMissingOrIncapableTools(t *testing.T) {
	tests := []struct {
		name      string
		git       func(string) string
		want      []string
		missingGo bool
	}{
		{
			name: "missing git",
			want: []string{"required git", "No lease or tab was created", "will not install"},
		},
		{
			name: "old git lacks exact merge-tree capability",
			git: func(dir string) string {
				return writeReviewTool(t, dir, "git", `
if [ "$1" = "--version" ]; then printf 'git version 2.34.1\n'; exit 0; fi
printf 'error: unknown option merge-base\n' >&2
exit 129
`)
			},
			want: []string{"git version 2.34.1", "merge-tree --write-tree --merge-base=HEAD", "missing required capability", "No lease or tab was created"},
		},
		{
			name: "missing go", git: func(dir string) string { return capableReviewGit(t, dir) }, missingGo: true,
			want: []string{"required go", "No lease or tab was created", "will not install"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			gitPath := filepath.Join(root, "missing", "git")
			if tt.git != nil {
				gitPath = tt.git(filepath.Join(root, "git-bin"))
			}
			goPath := capableReviewGo(t, filepath.Join(root, "go-bin"))
			if tt.missingGo {
				goPath = filepath.Join(root, "missing", "go")
			}
			t.Setenv(envReviewGit, gitPath)
			t.Setenv(envReviewGo, goPath)
			_, err := preflightReviewToolchain(root)
			if err == nil {
				t.Fatal("an incomplete W4 toolchain must be refused")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal must contain %q, got: %v", want, err)
				}
			}
		})
	}
}

func TestAdmittedReviewToolchainReachesReviewerProcess(t *testing.T) {
	root := t.TempDir()
	gitPath := capableReviewGit(t, filepath.Join(root, "git-bin"))
	goPath := capableReviewGo(t, filepath.Join(root, "go-bin"))
	t.Setenv(envReviewGit, gitPath)
	t.Setenv(envReviewGo, goPath)

	toolchain, err := preflightReviewToolchain(root)
	if err != nil {
		t.Fatalf("capable toolchain refused: %v", err)
	}
	cmd := exec.Command("sh", "-c", `printf 'git_path=%s\n' "$(command -v git)"; git --version; printf 'go_path=%s\n' "$(command -v go)"; go version`)
	cmd.Env = mergeReviewTestEnv(os.Environ(), reviewerTabEnvironment(toolchain))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reviewer process could not execute admitted tools: %v\n%s", err, out)
	}
	for _, want := range []string{gitPath, "git version 2.53.0", goPath, "go version go1.25.1"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("reviewer process output missing %q:\n%s", want, out)
		}
	}
}

func mergeReviewTestEnv(base, overrides []string) []string {
	keys := make(map[string]bool, len(overrides))
	for _, entry := range overrides {
		key, _, _ := strings.Cut(entry, "=")
		keys[key] = true
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if !keys[key] {
			out = append(out, entry)
		}
	}
	return append(out, overrides...)
}

func TestReviewToolchainPreflightIsBeforeLeaseAndTab(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := funcBody(string(src), "func runPoolReview")
	if !ok {
		t.Fatal("cannot locate runPoolReview")
	}
	preflight := strings.Index(body, "preflightReviewToolchain(")
	lease := strings.Index(body, "p.Lease(")
	create := strings.Index(body, "herdr.TabCreate(")
	if preflight < 0 || lease < 0 || create < 0 {
		t.Fatalf("launch path is missing toolchain preflight=%d lease=%d tab=%d", preflight, lease, create)
	}
	if preflight > lease || preflight > create {
		t.Error("W4 toolchain capabilities must be proved before lease or tab creation")
	}
	if !strings.Contains(body, "reviewerTabEnvironment(toolchain)") {
		t.Error("the admitted toolchain must be injected into the reviewer tab, not used only by the launcher")
	}
}
