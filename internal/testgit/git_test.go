package testgit

import (
	"os"
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

func TestCommandReplacesInheritedEnvironment(t *testing.T) {
	inherited := map[string]string{
		"GIT_CONFIG_COUNT":      "2",
		"GIT_CONFIG_KEY_0":      "user.name",
		"GIT_CONFIG_VALUE_0":    "host user",
		"GIT_CONFIG_KEY_1":      "user.signingKey",
		"GIT_CONFIG_VALUE_1":    "host-key",
		"GNUPGHOME":             "/host/gnupg",
		"GCM_INTERACTIVE":       "always",
		"GCM_GUI_PROMPT":        "1",
		"GIT_ASKPASS":           "/host/askpass",
		"SSH_ASKPASS":           "/host/ssh-askpass",
		"GIT_TERMINAL_PROMPT":   "1",
		"GIT_EDITOR":            "/host/editor",
		"GIT_SEQUENCE_EDITOR":   "/host/sequence-editor",
		"GIT_PAGER":             "/host/git-pager",
		"PAGER":                 "/host/pager",
		"SSH_AUTH_SOCK":         "/host/agent.sock",
		"GPG_TTY":               "/host/tty",
		"GPG_AGENT_INFO":        "/host/gpg-agent",
		"GIT_SSH_COMMAND":       "/host/ssh-command",
		"GIT_SSH":               "/host/ssh",
		"GIT_SSH_VARIANT":       "ssh",
		"GIT_CREDENTIAL_HELPER": "/host/credential-helper",
		"SSH_ASKPASS_REQUIRE":   "force",
		"GIT_CONFIG_GLOBAL":     "/host/global",
		"GIT_CONFIG_SYSTEM":     "/host/system",
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
		"commit", "-m", "message",
	}
	if got := strings.Join(cmd.Args, "\x00"); got != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", cmd.Args, want)
	}
}
