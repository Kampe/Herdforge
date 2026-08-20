package testgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func envValues(env []string) map[string][]string {
	values := make(map[string][]string)
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		values[key] = append(values[key], value)
	}
	return values
}

func isRepositoryStateEnv(key string) bool {
	switch key {
	case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_OBJECT_DIRECTORY_RELATIVE", "GIT_GRAFT_FILE", "GIT_SHALLOW_FILE",
		"GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM":
		return true
	default:
		return false
	}
}

func TestCommandReplacesInheritedEnvironment(t *testing.T) {
	inherited := map[string]string{
		"GIT_CONFIG_COUNT":                 "2",
		"GIT_CONFIG_KEY_0":                 "user.name",
		"GIT_CONFIG_VALUE_0":               "host user",
		"GIT_CONFIG_KEY_1":                 "user.signingKey",
		"GIT_CONFIG_VALUE_1":               "host-key",
		"GNUPGHOME":                        "/host/gnupg",
		"GCM_INTERACTIVE":                  "always",
		"GCM_GUI_PROMPT":                   "1",
		"GIT_ASKPASS":                      "/host/askpass",
		"SSH_ASKPASS":                      "/host/ssh-askpass",
		"GIT_TERMINAL_PROMPT":              "1",
		"GIT_EDITOR":                       "/host/editor",
		"GIT_SEQUENCE_EDITOR":              "/host/sequence-editor",
		"GIT_PAGER":                        "/host/git-pager",
		"PAGER":                            "/host/pager",
		"SSH_AUTH_SOCK":                    "/host/agent.sock",
		"GPG_TTY":                          "/host/tty",
		"GPG_AGENT_INFO":                   "/host/gpg-agent",
		"GIT_SSH_COMMAND":                  "/host/ssh-command",
		"GIT_SSH":                          "/host/ssh",
		"GIT_SSH_VARIANT":                  "ssh",
		"GIT_CREDENTIAL_HELPER":            "/host/credential-helper",
		"SSH_ASKPASS_REQUIRE":              "force",
		"GIT_CONFIG_GLOBAL":                "/host/global",
		"GIT_CONFIG_SYSTEM":                "/host/system",
		"GIT_DIR":                          "/host/repository",
		"GIT_WORK_TREE":                    "/host/worktree",
		"GIT_COMMON_DIR":                   "/host/common",
		"GIT_INDEX_FILE":                   "/host/index",
		"GIT_OBJECT_DIRECTORY":             "/host/objects",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": "/host/alternates",
		"GIT_OBJECT_DIRECTORY_RELATIVE":    "1",
		"GIT_GRAFT_FILE":                   "/host/grafts",
		"GIT_SHALLOW_FILE":                 "/host/shallow",
		"GIT_NAMESPACE":                    "host",
		"GIT_CEILING_DIRECTORIES":          "/host",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM":  "1",
	}
	for key, value := range inherited {
		t.Setenv(key, value)
	}

	fixtureDir := "fixture"
	cmd := Command(fixtureDir, "status", "--short")
	if cmd.Dir != fixtureDir {
		t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, fixtureDir)
	}
	values := envValues(cmd.Env)

	for key := range inherited {
		if strings.HasPrefix(key, "GIT_CONFIG_") && key != "GIT_CONFIG_GLOBAL" && key != "GIT_CONFIG_SYSTEM" {
			if got := values[key]; len(got) != 0 {
				t.Fatalf("%s entries = %v, want filtered", key, got)
			}
			continue
		}
		if isRepositoryStateEnv(key) {
			if got := values[key]; len(got) != 0 {
				t.Fatalf("%s entries = %v, want filtered", key, got)
			}
			continue
		}
		if got := values[key]; len(got) != 1 {
			t.Fatalf("%s entries = %v, want one replacement", key, got)
		}
	}
	want := map[string]string{
		"GIT_CONFIG_GLOBAL":     os.DevNull,
		"GIT_CONFIG_SYSTEM":     os.DevNull,
		"GIT_CONFIG_NOSYSTEM":   "1",
		"GNUPGHOME":             "",
		"GCM_INTERACTIVE":       "0",
		"GCM_GUI_PROMPT":        "0",
		"GIT_ASKPASS":           "",
		"SSH_ASKPASS":           "",
		"GIT_TERMINAL_PROMPT":   "0",
		"GIT_EDITOR":            ":",
		"GIT_SEQUENCE_EDITOR":   ":",
		"GIT_PAGER":             "cat",
		"PAGER":                 "cat",
		"SSH_AUTH_SOCK":         "",
		"GPG_TTY":               "",
		"GPG_AGENT_INFO":        "",
		"GIT_SSH_COMMAND":       "",
		"GIT_SSH":               "",
		"GIT_SSH_VARIANT":       "",
		"GIT_CREDENTIAL_HELPER": "",
		"SSH_ASKPASS_REQUIRE":   "never",
	}
	for key, expected := range want {
		if got := values[key]; len(got) != 1 || got[0] != expected {
			t.Fatalf("%s = %v, want [%q]", key, got, expected)
		}
	}
	for key := range inherited {
		if _, ok := values[key]; ok && isRepositoryStateEnv(key) {
			t.Fatalf("repository-state variable %s leaked into fixture environment", key)
		}
	}
	for key := range values {
		if strings.HasPrefix(key, "GIT_CONFIG_") && key != "GIT_CONFIG_GLOBAL" && key != "GIT_CONFIG_SYSTEM" && key != "GIT_CONFIG_NOSYSTEM" {
			t.Fatalf("inherited indexed Git config variable %s was retained", key)
		}
	}
}

func TestCommandUsesSafeGitArguments(t *testing.T) {
	cmd := Command("fixture", "commit", "-m", "message")
	want := []string{
		"git",
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "gpg.program=",
		"-c", "gpg.ssh.program=",
		"-c", "credential.helper=",
		"-c", "core.hooksPath=./.herd-test-no-hooks-do-not-create",
		"-c", "core.askPass=",
		"-c", "user.signingKey=",
		"-c", "user.name=Herdforge Test",
		"-c", "user.email=herdforge-test@example.invalid",
		"-c", "url.file:///dev/null.insteadOf=https://github.com/",
		"-c", "url.file:///dev/null.insteadOf=ssh://git@github.com/",
		"-c", "url.file:///dev/null.insteadOf=git@github.com:",
		"commit", "-m", "message",
	}
	if got := strings.Join(cmd.Args, "\x00"); got != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", cmd.Args, want)
	}
}

func TestCommandIgnoresInheritedRepositorySelection(t *testing.T) {
	fixture := t.TempDir()
	foreign := t.TempDir()
	for _, dir := range []string{fixture, foreign} {
		if out, err := Command(dir, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
			t.Fatalf("init %s: %v\n%s", dir, err, out)
		}
	}

	foreignGitDir, err := filepath.Abs(filepath.Join(foreign, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", foreignGitDir)
	t.Setenv("GIT_WORK_TREE", foreign)

	out, err := Command(fixture, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("fixture git command failed: %v", err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("resolve reported fixture root: %v", err)
	}
	want, err := filepath.EvalSymlinks(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("git selected %q, want fixture %q", got, want)
	}
}

func TestCommandRedirectsGitHubRemotesAwayFromTheNetwork(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "https", url: "https://github.com/Kampe/Herdforge.git"},
		{name: "ssh URL", url: "ssh://git@github.com/Kampe/Herdforge.git"},
		{name: "scp", url: "git@github.com:Kampe/Herdforge.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if out, err := Command(dir, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
				t.Fatalf("init fixture: %v\n%s", err, out)
			}
			if out, err := Command(dir, "remote", "add", "origin", tt.url).CombinedOutput(); err != nil {
				t.Fatalf("add remote: %v\n%s", err, out)
			}

			// This command would contact the real GitHub remote without the
			// rewrite. The /dev/null diagnostic proves Git resolved the URL
			// to the sink instead of merely failing for an unrelated reason.
			out, err := Command(dir, "ls-remote", "origin").CombinedOutput()
			if err == nil {
				t.Fatalf("GitHub remote unexpectedly succeeded: %s", out)
			}
			if !strings.Contains(string(out), "/dev/null") {
				t.Fatalf("GitHub remote was not redirected to sink: %v\n%s", err, out)
			}
		})
	}
}

func TestCommandCommitIgnoresHostileGlobalSigningConfig(t *testing.T) {
	global := filepath.Join(t.TempDir(), "hostile.gitconfig")
	if err := os.WriteFile(global, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = /bin/false\n[user]\n\tname = Host\n\temail = host@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if out, err := Command(dir, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("init fixture: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.txt"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := Command(dir, "add", "fixture.txt").CombinedOutput(); err != nil {
		t.Fatalf("stage fixture: %v\n%s", err, out)
	}

	// Establish that the hostile configuration really blocks an unguarded
	// commit; otherwise this regression test could pass vacuously.
	unguarded := exec.Command("git", "commit", "-q", "-m", "unguarded")
	unguarded.Dir = dir
	unguarded.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+global, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := unguarded.CombinedOutput(); err == nil {
		t.Fatalf("unguarded commit unexpectedly succeeded under hostile signing config\n%s", out)
	}

	guarded := Command(dir, "commit", "-q", "-m", "guarded")
	if out, err := guarded.CombinedOutput(); err != nil {
		t.Fatalf("hermetic commit failed under hostile signing config: %v\n%s", err, out)
	}
	if out, err := Command(dir, "log", "-1", "--format=%s").Output(); err != nil || strings.TrimSpace(string(out)) != "guarded" {
		t.Fatalf("guarded commit was not recorded: %v %q", err, out)
	}
}
