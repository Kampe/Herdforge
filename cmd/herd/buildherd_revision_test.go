package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestValidateCLITestRevision(t *testing.T) {
	sha1 := "0123456789abcdef0123456789abcdef01234567"
	sha256 := strings.Repeat("ab", 32)
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "sha1", raw: sha1, want: sha1},
		{name: "sha1 trimmed", raw: " " + sha1 + "\n", want: sha1},
		{name: "sha256", raw: sha256, want: sha256},
		{name: "porcelain dirty pair", raw: "!! fake.go\n?? new.txt\n", wantErr: true},
		{name: "porcelain single", raw: "!! fake.go\n", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
		{name: "whitespace", raw: " \n", wantErr: true},
		{name: "short hex", raw: "abc", wantErr: true},
		{name: "internal space", raw: "0123456789abcdef 0123456789abcdef01234567", wantErr: true},
		{name: "non hex", raw: strings.Repeat("g", 40), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateCLITestRevision(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validateCLITestRevision(%q) succeeded with %q", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateCLITestRevision(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("validateCLITestRevision(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestGitBinaryOnPATHMissingFailsClosed(t *testing.T) {
	if _, err := gitBinaryOnPATH(""); err == nil {
		t.Fatal("empty PATH must fail closed")
	}
	if _, err := gitBinaryOnPATH(t.TempDir()); err == nil {
		t.Fatal("PATH without git must fail closed")
	}
}

func TestCLITestRevisionIgnoresFakeGitOnPATH(t *testing.T) {
	dir := t.TempDir()
	installFakeGit(t, dir, "!! fake.go\n?? new.txt\n")

	poisoned, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("PATH git: %v", err)
	}
	if !strings.Contains(string(poisoned), "fake.go") {
		t.Fatalf("fixture did not poison PATH git: %q", poisoned)
	}

	got, err := cliTestRevision()
	if err != nil {
		t.Fatalf("cliTestRevision: %v", err)
	}
	want, err := validateCLITestRevision(string(mustCapturedGitHEAD(t)))
	if err != nil {
		t.Fatalf("captured git HEAD: %v", err)
	}
	if got != want {
		t.Fatalf("cliTestRevision = %q, want captured HEAD %q (PATH git was %q)", got, want, poisoned)
	}
}

func mustCapturedGitHEAD(t *testing.T) []byte {
	t.Helper()
	if cliTestGitErr != nil {
		t.Fatalf("captured git: %v", cliTestGitErr)
	}
	if strings.TrimSpace(cliTestGit) == "" {
		t.Fatal("captured git path is empty")
	}
	out, err := exec.Command(cliTestGit, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("captured git rev-parse HEAD: %v", err)
	}
	return out
}
