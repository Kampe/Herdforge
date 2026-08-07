package security

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Env keys the sandbox may inject. Anything else from ambient is dropped.
const (
	EnvSandbox        = "HERD_SANDBOX"
	EnvSandboxRole    = "HERD_SANDBOX_ROLE"
	EnvSandboxCWD     = "HERD_SANDBOX_CWD"
	EnvSandboxAuth    = "HERD_SANDBOX_AUTHORITY"
	EnvSandboxTools   = "HERD_SANDBOX_TOOLS"
	EnvSandboxNet     = "HERD_SANDBOX_NETWORK"
	EnvSandboxRepo    = "HERD_SANDBOX_REPO"
	EnvSandboxLinks   = "HERD_SANDBOX_EXTERNAL_LINKS"
	EnvSandboxPkgs    = "HERD_SANDBOX_PACKAGES"
	EnvCredMode       = "HERD_CRED_MODE"
	EnvCredModeNone   = "none"
	EnvCredModeIntRef = "integration-ref" // reference only — never raw tokens
)

// ConstructAgentEnv builds the process environment for a sandboxed agent.
// Starts empty (not ambient) so integration/model/provider secrets cannot
// inherit. Only grant-backed capability flags and a minimal PATH are set.
// ambient is consulted solely for PATH (executable lookup), never for secrets.
func ConstructAgentEnv(grant *LaunchGrant, policy *LaunchPolicy, ambient map[string]string) ([]string, error) {
	if grant == nil || policy == nil {
		return nil, ErrUnknownPolicy
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if grant.CWD == "" || grant.CWD != policy.FilesystemRoot {
		return nil, fmt.Errorf("%w: grant cwd must equal policy filesystem root", ErrPathDenied)
	}

	// Scrub probe: ambient secrets must never appear in output.
	if err := AuthorizeEnvAgainstDeny(ambient, policy.SecretDeny); err != nil {
		// Not an error to *have* ambient secrets in the parent process —
		// we simply must not copy them. Record and continue scrubbed.
		policy.record(EventDenial, "ambient_secret_scrubbed", err.Error())
	}

	env := map[string]string{
		EnvSandbox:      "1",
		EnvSandboxRole:  grant.Role,
		EnvSandboxCWD:   grant.CWD,
		EnvSandboxAuth:  string(grant.Authority),
		EnvSandboxTools: strings.Join(grant.AllowedTools, ","),
		EnvSandboxNet:   grant.Network,
		EnvSandboxRepo:  policy.RepoIdentity,
		EnvSandboxLinks: string(policy.ExternalLinks),
		EnvCredMode:     EnvCredModeNone,
		"HOME":          grant.CWD,
		"PWD":           grant.CWD,
		"TMPDIR":        filepath.Join(grant.CWD, ".tmp"),
	}
	if len(grant.PackageRoots) > 0 {
		env[EnvSandboxPkgs] = strings.Join(grant.PackageRoots, ",")
	}
	if policy.IntegrationCredentials {
		// Reference mode only — still no raw tokens in the child env.
		env[EnvCredMode] = EnvCredModeIntRef
	}
	// Minimal PATH for tool binaries (from ambient PATH only, never full env).
	if ambient != nil {
		if p := ambient["PATH"]; p != "" {
			env["PATH"] = p
		}
	}
	if env["PATH"] == "" {
		env["PATH"] = "/usr/bin:/bin"
	}
	// Limited mode: HTTP(S)_PROXY embeds Basic userinfo (standard clients).
	// Do NOT set NO_PROXY for localhost — that would bypass the broker.
	if grant != nil && strings.EqualFold(grant.Network, "limited") && ambient != nil {
		if b := ambient["HERD_NETWORK_BROKER"]; b != "" {
			env["HTTP_PROXY"] = b
			env["HTTPS_PROXY"] = b
			env["http_proxy"] = b
			env["https_proxy"] = b
		}
		// Public MITM CA for HostCreds CONNECT (never private key / API secrets).
		if ca := ambient["SSL_CERT_FILE"]; ca != "" {
			env["SSL_CERT_FILE"] = ca
			env["NODE_EXTRA_CA_CERTS"] = ca
			env["REQUESTS_CA_BUNDLE"] = ca
			env["CURL_CA_BUNDLE"] = ca
			env["SSL_CERT_DIR"] = filepath.Dir(ca)
		}
	}
	// Sealed control path + exact binding for wrapper start barrier (never MAC secret).
	if ambient != nil {
		for _, k := range []string{"HERD_SEALED_CONTROL", "HERD_EXPECTED_TASK", "HERD_EXPECTED_LEASE", "HERD_EXPECTED_WORKER", "HERD_SEAL_WAIT"} {
			if s := ambient[k]; s != "" {
				env[k] = s
			}
		}
	}

	// Final scrub of constructed map against deny list + secret patterns.
	if err := policy.AuthorizeEnv(env); err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out, nil
}

// EnvironMap parses os.Environ-style slices or builds from os.Environ().
func EnvironMap(environ []string) map[string]string {
	if environ == nil {
		environ = os.Environ()
	}
	m := make(map[string]string, len(environ))
	for _, e := range environ {
		if i := strings.IndexByte(e, '='); i > 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}

// AuthorizeEnvAgainstDeny reports ErrSecretPresent if any denied name is set.
func AuthorizeEnvAgainstDeny(env map[string]string, deny []string) error {
	if env == nil {
		return nil
	}
	for _, name := range deny {
		if v, ok := env[name]; ok && strings.TrimSpace(v) != "" {
			return fmt.Errorf("%w: %s", ErrSecretPresent, name)
		}
	}
	return nil
}

// EnvHasSecret reports whether a KEY=VALUE list contains a forbidden secret
// (mutation probe for ConstructAgentEnv output).
func EnvHasSecret(env []string, names ...string) bool {
	want := map[string]struct{}{}
	for _, n := range names {
		want[n] = struct{}{}
	}
	for _, e := range env {
		i := strings.IndexByte(e, '=')
		if i <= 0 {
			continue
		}
		k, v := e[:i], e[i+1:]
		if _, ok := want[k]; ok && strings.TrimSpace(v) != "" {
			return true
		}
		uk := strings.ToUpper(k)
		if strings.Contains(uk, "API_KEY") || strings.Contains(uk, "API_TOKEN") ||
			strings.HasSuffix(uk, "_PAT") || strings.Contains(uk, "MERGE_TOKEN") {
			if strings.TrimSpace(v) != "" {
				return true
			}
		}
	}
	return false
}
