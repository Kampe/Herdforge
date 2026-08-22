package herdr

import (
	"os"
	"os/exec"
	"strings"
	"sync"
)

// FAC-558: a managed lane inherits the launcher's environment, and a long-lived
// shell often exports a GOROOT from a different Go install than the `go` on
// PATH. Every Go command then fails on mixed tool versions, and the standing
// workaround is `env -u GOROOT ...` on every invocation. A workaround every
// operator must remember is a launch-environment defect, not operator error.
//
// The lane environment is delivered through `herdr tab create --env KEY=VALUE`,
// which can only SET a variable. Unsetting an inherited one is not expressible,
// so neutralizing a stale GOROOT means PINNING it to the value the PATH-resolved
// toolchain reports for itself. Same for GOTOOLDIR, which is equally stale and
// equally fatal.
//
// This applies only to lanes Herdforge launches. An operator who exports GOROOT
// in their own shell still trips the preflight mismatch diagnostic, which is the
// intended behaviour: we normalize what we own and keep diagnosing what we do
// not.

var (
	goToolchainOnce sync.Once
	goToolchainEnv  []string
)

// GoToolchainEnv returns KEY=VALUE entries that pin a lane to the PATH-resolved
// Go toolchain, or nil when nothing needs pinning or the toolchain cannot be
// resolved.
//
// Resolution is attempted once per process: it shells out, and every lane launch
// would otherwise repeat it.
func GoToolchainEnv() []string {
	goToolchainOnce.Do(func() { goToolchainEnv = resolveGoToolchainEnv() })
	return goToolchainEnv
}

func resolveGoToolchainEnv() []string {
	// Nothing exported means nothing stale to override. Injecting a value here
	// would be inventing a toolchain choice the operator did not make.
	if strings.TrimSpace(os.Getenv("GOROOT")) == "" && strings.TrimSpace(os.Getenv("GOTOOLDIR")) == "" {
		return nil
	}
	if _, err := exec.LookPath("go"); err != nil {
		// No Go on PATH: there is no authoritative value to pin to, so leave
		// the environment alone and let preflight report the mismatch.
		return nil
	}
	var pinned []string
	for _, key := range []string{"GOROOT", "GOTOOLDIR"} {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			continue
		}
		value, ok := pathResolvedGoEnv(key)
		if !ok {
			// Fail open rather than pin a guess: a wrong GOROOT is worse than
			// the stale one, and preflight still diagnoses the stale case.
			continue
		}
		pinned = append(pinned, key+"="+value)
	}
	return pinned
}

// pathResolvedGoEnv asks the PATH-resolved go binary what a variable should be,
// with the stale values removed from its own environment so it reports its
// actual root rather than echoing what it was handed.
func pathResolvedGoEnv(key string) (string, bool) {
	cmd := exec.Command("go", "env", key)
	base := os.Environ()
	env := make([]string, 0, len(base))
	for _, entry := range base {
		if strings.HasPrefix(entry, "GOROOT=") || strings.HasPrefix(entry, "GOTOOLDIR=") {
			continue
		}
		env = append(env, entry)
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", false
	}
	return value, true
}

// withGoToolchainPinned appends the toolchain pins unless the caller already
// set one explicitly. An explicit choice by a caller wins over normalization.
func withGoToolchainPinned(env []string) []string {
	pins := GoToolchainEnv()
	if len(pins) == 0 {
		return env
	}
	out := append([]string(nil), env...)
	for _, pin := range pins {
		key, _, _ := strings.Cut(pin, "=")
		explicit := false
		for _, entry := range env {
			if k, _, ok := strings.Cut(entry, "="); ok && k == key {
				explicit = true
				break
			}
		}
		if !explicit {
			out = append(out, pin)
		}
	}
	return out
}

// resetGoToolchainMemoForTest clears the per-process resolution memo so a test
// can observe a different environment. Test-only.
func resetGoToolchainMemoForTest() {
	goToolchainOnce = sync.Once{}
	goToolchainEnv = nil
}
