package provenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadExecutableConsumerRepositoryDoesNotCompareUnrelatedRevisions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/consumer\n\ngo 1.25\n")
	gitRun(t, root, "init", "-b", "main")
	gitRun(t, root, "config", "user.email", "test@example.com")
	gitRun(t, root, "config", "user.name", "test")
	gitRun(t, root, "add", "go.mod")
	gitRun(t, root, "commit", "-m", "init")

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	// The test executable is built from Herdforge, while root is a consumer
	// repository. Its revisions must never be compared.
	info, err := ReadExecutable(executable, root)
	if err != nil {
		t.Fatalf("ReadExecutable() error = %v", err)
	}
	if info.Comparable {
		t.Fatal("consumer repository must not be comparable to the Herdforge binary")
	}
	if info.Current {
		t.Fatal("cross-repository provenance must not be reported as current")
	}
	got := Format(info)
	if strings.Contains(got, "herd provenance: STALE") {
		t.Fatalf("consumer provenance falsely reported stale:\n%s", got)
	}
	if !strings.Contains(got, "herd provenance: UNKNOWN") {
		t.Fatalf("consumer provenance must be unknown:\n%s", got)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestReadRejectsRedirectedGitRoot(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	redirect := filepath.Join(t.TempDir(), "other-worktree")
	cmd = exec.Command("git", "config", "--local", "core.worktree", redirect)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v\n%s", err, out)
	}
	_, err := Read(root)
	if err == nil || !strings.Contains(err.Error(), "core.worktree") || !strings.Contains(err.Error(), redirect) {
		t.Fatalf("Read() error = %v, want core.worktree redirect diagnostic", err)
	}
}

func TestValidateBinarySource(t *testing.T) {
	cases := []struct {
		name, source, binary string
		wantErr              string
	}{
		{"current", "abcdef0123456789", "abcdef0123456789", ""},
		{"stale", "abcdef0123456789", "1234567890abcdef", "stale herd binary"},
		{"missing metadata", "abcdef0123456789", "", "no source revision metadata"},
		{"missing source", "", "abcdef0123456789", "source revision is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Info{BinaryRevision: tc.binary}, tc.source)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestFormatReportsAllProvenanceFields(t *testing.T) {
	got := Format(Info{Path: "./bin/herd", SourceRevision: "source", BinaryRevision: "binary", BuildTime: "time", Comparable: true})
	for _, want := range []string{"binary path: ./bin/herd", "source revision: source", "binary build revision: binary", "binary build time: time", "STALE"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Format() = %q, missing %q", got, want)
		}
	}
}

func TestValidateInstalledInfosChecksEveryPath(t *testing.T) {
	cases := []struct {
		name  string
		infos []Info
		want  string
	}{
		{
			name: "all current",
			infos: []Info{
				{Path: "./herd", BinaryRevision: "source"},
				{Path: "./bin/herd", BinaryRevision: "source"},
			},
		},
		{
			name: "reports stale sibling",
			infos: []Info{
				{Path: "./herd", BinaryRevision: "source"},
				{Path: "./bin/herd", BinaryRevision: "old"},
			},
			want: "./bin/herd: stale herd binary",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInstalledInfos(tc.infos, "source")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateInstalledInfos() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateInstalledInfos() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestInstalledBinaryPathsAreDeterministic(t *testing.T) {
	got := InstalledBinaryPaths("/repo")
	want := []string{"/repo/herd", "/repo/bin/herd"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("InstalledBinaryPaths() = %v, want %v", got, want)
	}
}
