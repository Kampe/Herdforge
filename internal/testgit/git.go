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
		// Repository-selection and object-store variables must not leak into a
		// fixture. An inherited GIT_DIR/GIT_COMMON_DIR can make a worktree
		// command operate on another test's repository even when cmd.Dir is
		// pinned to the fixture path.
		"GIT_DIR": {}, "GIT_WORK_TREE": {}, "GIT_COMMON_DIR": {},
		"GIT_INDEX_FILE": {}, "GIT_OBJECT_DIRECTORY": {},
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": {}, "GIT_OBJECT_DIRECTORY_RELATIVE": {},
		"GIT_GRAFT_FILE": {}, "GIT_SHALLOW_FILE": {}, "GIT_NAMESPACE": {},
		"GIT_CEILING_DIRECTORIES": {}, "GIT_DISCOVERY_ACROSS_FILESYSTEM": {},
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
