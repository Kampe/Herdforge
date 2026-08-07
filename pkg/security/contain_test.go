package security

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDarwinSeatbelt_ProveDenials_RealChild(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	sb := DarwinSeatbelt{}
	if !sb.Available() {
		t.Fatal("sandbox-exec required")
	}
	// Shared checkout with worktree underneath — hermetic temp tree only.
	shared := t.TempDir()
	shared, _ = filepath.Abs(shared)
	// Non-mutating shared file (not under worktree) for sibling-read proof.
	if err := os.WriteFile(filepath.Join(shared, "go.mod"), []byte("module probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(shared, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := StructureTask("FAC-133", "t", "d", RoleWorker, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file", "git-write"},
		Structured: st, ProviderText: "d", Env: map[string]string{"PATH": "/bin:/usr/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pathPrefix, profile, envFile, err := sb.Install(wt, p, grant, "true", "/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if pathPrefix == "" || profile == "" || envFile == "" {
		t.Fatal("install returned empty paths")
	}
	env, err := ConstructAgentEnv(grant, p, map[string]string{"PATH": "/bin:/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteEnvFile(envFile, env); err != nil {
		t.Fatal(err)
	}
	if err := sb.ProveDenials(wt, p, grant, profile, env); err != nil {
		t.Fatalf("ProveDenials: %v", err)
	}
	// Hermetic: no crash residue under shared root (only go.mod we wrote).
	entries, _ := os.ReadDir(shared)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".fac133") {
			t.Fatalf("shared checkout polluted with %s", name)
		}
	}
}

func TestDarwinSeatbelt_DenySiblingAndHomeCredentials(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	sb := DarwinSeatbelt{}
	if !sb.Available() {
		t.Fatal("sandbox-exec required")
	}
	shared := t.TempDir()
	shared, _ = filepath.Abs(shared)
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	// Sibling repo next to shared (outside worktree).
	sibling := filepath.Join(filepath.Dir(shared), "sibling-other-repo")
	_ = os.MkdirAll(sibling, 0o755)
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })
	secretPath := filepath.Join(sibling, "creds.txt")
	if err := os.WriteFile(secretPath, []byte("SIBLING_SECRET_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := StructureTask("FAC-133", "t", "d", RoleWorker, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file"},
		Structured: st, ProviderText: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, profile, envFile, err := sb.Install(wt, p, grant, "true", "/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	env, _ := ConstructAgentEnv(grant, p, map[string]string{"PATH": "/bin:/usr/bin"})
	_ = WriteEnvFile(envFile, env)

	out, err := runContained(profile, env, wt, "/bin/bash", "-c", "cat "+shellQuote(secretPath)+" 2>&1 || true")
	if strings.Contains(out, "SIBLING_SECRET_MARKER") {
		t.Fatalf("sibling repo readable under seatbelt: %q err=%v", out, err)
	}

	// Real home .ssh if present — must not leak.
	if home, herr := os.UserHomeDir(); herr == nil {
		sshDir := filepath.Join(home, ".ssh")
		if st, _ := os.Stat(sshDir); st != nil && st.IsDir() {
			out, _ = runContained(profile, env, wt, "/bin/bash", "-c", "ls "+shellQuote(sshDir)+" 2>&1 || true")
			// Success with listing would be a failure; empty or Operation not permitted is ok.
			if err == nil && out != "" && !strings.Contains(out, "Operation not permitted") &&
				!strings.Contains(out, "Permission denied") && !strings.Contains(out, "Sandbox") {
				// If ls produced names of keys, fail.
				if strings.Contains(out, "id_rsa") || strings.Contains(out, "id_ed25519") || strings.Contains(out, "known_hosts") {
					t.Fatalf("home .ssh readable: %q", out)
				}
			}
		}
	}
}

func TestDarwinSeatbelt_ForbiddenToolExecDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	if _, err := os.Stat("/usr/bin/curl"); err != nil {
		t.Skip("curl not present")
	}
	sb := DarwinSeatbelt{}
	shared := t.TempDir()
	shared, _ = filepath.Abs(shared)
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := StructureTask("FAC-133", "t", "d", RoleWorker, wt, "", "", false)
	// No curl/network in tools.
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file"},
		Structured: st, ProviderText: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, profile, envFile, err := sb.Install(wt, p, grant, "true", "/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	env, _ := ConstructAgentEnv(grant, p, map[string]string{"PATH": "/bin:/usr/bin"})
	_ = WriteEnvFile(envFile, env)
	_, err = runContained(profile, env, wt, "/usr/bin/curl", "--help")
	if err == nil {
		t.Fatal("curl --help must be denied when not on AllowedTools exec allowlist")
	}
}

func TestDarwinSeatbelt_OfflineNetworkDenied(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	sb := DarwinSeatbelt{}
	shared := t.TempDir()
	shared, _ = filepath.Abs(shared)
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	p, err := PolicyForLane(RoleReviewer, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := StructureTask("FAC-133", "t", "d", RoleReviewer, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleReviewer, Tools: []string{"read-file", "git-read"},
		Structured: st, ProviderText: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	if grant.Network != "limited" && grant.Network != "offline" {
		t.Fatalf("expected limited/offline network, got %s", grant.Network)
	}
	_, profile, envFile, err := sb.Install(wt, p, grant, "true", "/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	env, _ := ConstructAgentEnv(grant, p, map[string]string{"PATH": "/bin:/usr/bin"})
	_ = WriteEnvFile(envFile, env)
	if err := sb.ProveDenials(wt, p, grant, profile, env); err != nil {
		t.Fatalf("reviewer ProveDenials: %v", err)
	}
}

func TestDarwinSeatbelt_ExclusivePackage(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	shared := t.TempDir()
	shared, _ = filepath.Abs(shared)
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(filepath.Join(wt, "pkg", "envelope"), 0o755)
	_ = os.MkdirAll(filepath.Join(wt, "pkg", "secrets"), 0o755)
	_ = os.WriteFile(filepath.Join(wt, "pkg", "secrets", "x.go"), []byte("package secrets"), 0o644)

	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", []string{"pkg/envelope"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.ExclusivePackages {
		t.Fatal("expected exclusive packages")
	}
	st := StructureTask("FAC-133", "t", "d", RoleWorker, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file"},
		Paths: []string{filepath.Join(wt, "pkg", "envelope", "a.go")},
		Structured: st, ProviderText: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Path authorize for secrets should fail at policy layer.
	if err := p.AuthorizePath(filepath.Join(wt, "pkg", "secrets", "x.go")); err == nil {
		t.Fatal("policy must deny non-allowlisted package path")
	}
	_, profile, envFile, err := DarwinSeatbelt{}.Install(wt, p, grant, "true", "/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	env, _ := ConstructAgentEnv(grant, p, map[string]string{"PATH": "/bin:/usr/bin"})
	_ = WriteEnvFile(envFile, env)
	// Reading secrets package under seatbelt must fail.
	out, err := runContained(profile, env, wt, "/bin/bash", "-c", "cat "+shellQuote(filepath.Join(wt, "pkg", "secrets", "x.go")))
	if err == nil && strings.Contains(out, "package secrets") {
		t.Fatalf("seatbelt allowed exclusive package escape: %s", out)
	}
}

func TestWriteEnvFile_PreservesSpaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env.list")
	env := []string{
		"PATH=/bin:/usr/bin",
		"NOTE=hello world with spaces",
		"META=a=b;c`d$",
	}
	if err := WriteEnvFile(path, env); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "NOTE=hello world with spaces\n") {
		t.Fatalf("spaces not preserved: %q", got)
	}
	if !strings.Contains(got, "META=a=b;c`d$\n") {
		t.Fatalf("metacharacters not preserved: %q", got)
	}
	// Reject newlines.
	if err := WriteEnvFile(path, []string{"BAD=line\nfeed"}); err == nil {
		t.Fatal("expected reject newline in env value")
	}
}

func TestWriteEnvFile_RoundTripUnderSeatbelt(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is Darwin-only")
	}
	sb := DarwinSeatbelt{}
	shared := t.TempDir()
	shared, _ = filepath.Abs(shared)
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	p, err := PolicyForLane(RoleWorker, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := StructureTask("FAC-133", "t", "d", RoleWorker, wt, "", "", false)
	grant, err := p.AuthorizeLaunch(LaunchRequest{
		CWD: wt, Role: RoleWorker, Tools: []string{"read-file"},
		Structured: st, ProviderText: "d",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, profile, envFile, err := sb.Install(wt, p, grant, "true", "/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	env, err := ConstructAgentEnv(grant, p, map[string]string{"PATH": "/bin:/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	// Inject a value with spaces through the same transport the wrapper uses.
	env = append(env, "FAC133_SPACE_VAL=hello world spaces")
	if err := WriteEnvFile(envFile, env); err != nil {
		t.Fatal(err)
	}
	// Child sees the value via env -i from WrapCmd (same as production scrubbed env).
	out, err := runContained(profile, env, wt, "/bin/bash", "-c", `printf '%s' "$FAC133_SPACE_VAL"`)
	if err != nil {
		t.Fatalf("runContained: %v out=%q", err, out)
	}
	if out != "hello world spaces" {
		t.Fatalf("env space value corrupted: %q", out)
	}
}

func TestRequireContainment_FailClosed(t *testing.T) {
	b, err := RequireContainment()
	if runtime.GOOS == "darwin" {
		if err != nil || b == nil {
			t.Fatalf("darwin should have sandbox-exec: %v", err)
		}
		if b.Name() != "sandbox-exec" {
			t.Fatalf("name=%s", b.Name())
		}
		return
	}
	// Non-darwin: must not advertise stub bwrap.
	if err == nil {
		t.Fatalf("non-darwin must fail closed without full backend, got %s", b.Name())
	}
}

func TestActiveContainment_NoLinuxStub(t *testing.T) {
	if runtime.GOOS == "linux" {
		b := ActiveContainment()
		if b != nil {
			t.Fatal("linux must not advertise incomplete bwrap backend")
		}
	}
}

func TestSeatbeltProfile_IsDenyDefault(t *testing.T) {
	prof := seatbeltProfileDenyDefault("/wt", "/shared", "/usr/bin/true", &LaunchGrant{
		Role: RoleWorker, Network: "online", AllowedTools: []string{"read-file"},
	}, &LaunchPolicy{SharedCheckout: "/shared"})
	if !strings.Contains(prof, "(deny default)") {
		t.Fatal("profile must deny default")
	}
	if strings.Contains(prof, "(allow default)") {
		t.Fatal("profile must not allow default")
	}
	if !strings.Contains(prof, "(deny file-read*") {
		t.Fatal("profile must include explicit credential/shared denials")
	}
}
