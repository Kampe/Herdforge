// Package testgit provides the bounded Git command environment used by test
// fixtures. It is intentionally not used by production code.
package testgit

import (
	"os"
	"os/exec"
	"strings"
)

// Command creates a Git command that cannot consult developer signing, UI, or
// credential configuration. All paths in the policy are relative to the
// fixture repository or are process controls, never host-specific helpers.
func Command(dir string, args ...string) *exec.Cmd {
	config := []string{
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
		// A fixture must never be able to turn an accidentally inherited
		// GitHub origin into a real network read or push. Local bare remotes
		// used by tests do not match these URL forms and remain usable.
		"-c", "url.file:///dev/null.insteadOf=https://github.com/",
		"-c", "url.file:///dev/null.insteadOf=ssh://git@github.com/",
		"-c", "url.file:///dev/null.insteadOf=git@github.com:",
	}
	cmd := exec.Command("git", append(config, args...)...)
	cmd.Dir = dir
	blocked := map[string]struct{}{
		"GIT_CONFIG_GLOBAL": {}, "GIT_CONFIG_SYSTEM": {}, "GIT_CONFIG_NOSYSTEM": {},
		"GNUPGHOME": {}, "GCM_INTERACTIVE": {}, "GCM_GUI_PROMPT": {},
		"GIT_ASKPASS": {}, "SSH_ASKPASS": {}, "GIT_TERMINAL_PROMPT": {},
		"GIT_EDITOR": {}, "GIT_SEQUENCE_EDITOR": {}, "GIT_PAGER": {}, "PAGER": {},
		"SSH_AUTH_SOCK": {}, "GPG_TTY": {}, "GPG_AGENT_INFO": {},
		"GIT_SSH_COMMAND": {}, "GIT_SSH": {}, "GIT_SSH_VARIANT": {},
		"GIT_CREDENTIAL_HELPER": {}, "SSH_ASKPASS_REQUIRE": {},
	}
	env := make([]string, 0, len(os.Environ())+12)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "GIT_CONFIG_") {
			continue
		}
		if _, skip := blocked[key]; !skip {
			env = append(env, item)
		}
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GNUPGHOME=", "GCM_INTERACTIVE=0", "GCM_GUI_PROMPT=0",
		"GIT_ASKPASS=", "SSH_ASKPASS=", "GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=:", "GIT_SEQUENCE_EDITOR=:", "GIT_PAGER=cat", "PAGER=cat",
		"SSH_AUTH_SOCK=", "GPG_TTY=", "GPG_AGENT_INFO=",
		"GIT_SSH_COMMAND=", "GIT_SSH=", "GIT_SSH_VARIANT=",
		"GIT_CREDENTIAL_HELPER=", "SSH_ASKPASS_REQUIRE=never",
	)
	cmd.Env = env
	return cmd
}
