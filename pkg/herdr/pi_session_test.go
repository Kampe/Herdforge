package herdr

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
)

var exactPiHarnessArgv = []string{"pi", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium"}

func writePiSession(t *testing.T, root, name, body string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func piSessionBody(provider, model, thinking string) string {
	return `{"type":"session","version":3,"cwd":"/repo"}` + "\n" +
		`{"type":"model_change","provider":"` + provider + `","modelId":"` + model + `"}` + "\n" +
		`{"type":"thinking_level_change","thinkingLevel":"` + thinking + `"}` + "\n"
}

func configurePiSessionRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", root)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	return root
}

func TestAttestPiSessionRouteExact(t *testing.T) {
	root := configurePiSessionRoot(t)
	path := writePiSession(t, root, "exact.jsonl", piSessionBody("openai-codex", "gpt-5.6-luna", "medium"), 0o600)
	if err := attestPiSessionRoute(path, exactPiHarnessArgv); err != nil {
		t.Fatal(err)
	}
}

func TestAttestPiSessionRouteFailsClosed(t *testing.T) {
	root := configurePiSessionRoot(t)
	valid := writePiSession(t, root, "valid.jsonl", piSessionBody("openai-codex", "gpt-5.6-luna", "medium"), 0o600)
	check := func(t *testing.T, err error, target error) {
		t.Helper()
		if err == nil || !errors.Is(err, target) {
			t.Fatalf("error=%v want %v", err, target)
		}
	}
	t.Run("missing", func(t *testing.T) {
		check(t, attestPiSessionRoute(filepath.Join(root, "missing.jsonl"), exactPiHarnessArgv), ErrPiSessionNotReady)
	})
	t.Run("incomplete", func(t *testing.T) {
		path := writePiSession(t, root, "incomplete.jsonl", `{"type":"session","version":3}`+"\n", 0o600)
		check(t, attestPiSessionRoute(path, exactPiHarnessArgv), ErrPiSessionNotReady)
	})
	for name, body := range map[string]string{
		"provider": piSessionBody("anthropic", "gpt-5.6-luna", "medium"),
		"model":    piSessionBody("openai-codex", "gpt-5.6-sol", "medium"),
		"thinking": piSessionBody("openai-codex", "gpt-5.6-luna", "high"),
	} {
		t.Run(name, func(t *testing.T) {
			path := writePiSession(t, root, name+".jsonl", body, 0o600)
			err := attestPiSessionRoute(path, exactPiHarnessArgv)
			check(t, err, ErrPiSessionRouteMismatch)
			if !strings.Contains(err.Error(), "got") {
				t.Fatalf("mismatch lacks got/want: %v", err)
			}
		})
	}
	t.Run("malformed", func(t *testing.T) {
		path := writePiSession(t, root, "malformed.jsonl", "{\n", 0o600)
		check(t, attestPiSessionRoute(path, exactPiHarnessArgv), ErrPiSessionRouteMismatch)
	})
	t.Run("world writable", func(t *testing.T) {
		path := writePiSession(t, root, "writable.jsonl", piSessionBody("openai-codex", "gpt-5.6-luna", "medium"), 0o666)
		check(t, attestPiSessionRoute(path, exactPiHarnessArgv), ErrPiSessionRouteMismatch)
	})
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(root, "link.jsonl")
		if err := os.Symlink(valid, link); err != nil {
			t.Fatal(err)
		}
		check(t, attestPiSessionRoute(link, exactPiHarnessArgv), ErrPiSessionRouteMismatch)
	})
	t.Run("outside", func(t *testing.T) {
		outside := writePiSession(t, t.TempDir(), "outside.jsonl", piSessionBody("openai-codex", "gpt-5.6-luna", "medium"), 0o600)
		check(t, attestPiSessionRoute(outside, exactPiHarnessArgv), ErrPiSessionRouteMismatch)
	})
	t.Run("relative", func(t *testing.T) {
		check(t, attestPiSessionRoute("relative.jsonl", exactPiHarnessArgv), ErrPiSessionRouteMismatch)
	})
	for name, argv := range map[string][]string{
		"nil":          nil,
		"model suffix": {"pi", "--model", "openai-codex/", "--thinking", "medium"},
		"wrong flag":   {"pi", "--provider", "openai-codex/gpt-5.6-luna", "--thinking", "medium"},
		"thinking":     {"pi", "--model", "openai-codex/gpt-5.6-luna", "--thinking", ""},
	} {
		t.Run("argv "+name, func(t *testing.T) { check(t, attestPiSessionRoute(valid, argv), ErrPiSessionRouteMismatch) })
	}
}

func TestPiSessionRouteAttesterSetter(t *testing.T) {
	want := errors.New("injected attester")
	called := false
	restore := SetPiSessionRouteAttesterForTest(func(path string, argv []string) error {
		called = path == "session" && len(argv) == len(exactPiHarnessArgv)
		return want
	})
	if err := verifyPiSessionRoute("session", exactPiHarnessArgv); !errors.Is(err, want) || !called {
		t.Fatalf("setter err=%v called=%v", err, called)
	}
	restore()
	root := configurePiSessionRoot(t)
	path := writePiSession(t, root, "restored.jsonl", piSessionBody("openai-codex", "gpt-5.6-luna", "medium"), 0o600)
	if err := verifyPiSessionRoute(path, exactPiHarnessArgv); err != nil {
		t.Fatalf("production attester not restored: %v", err)
	}
}

func TestAttestPiSessionRouteBound(t *testing.T) {
	root := configurePiSessionRoot(t)
	path := writePiSession(t, root, "bound.jsonl", piSessionBody("openai-codex", "gpt-5.6-luna", "medium"), 0o600)
	argv := append(append([]string(nil), exactPiHarnessArgv...), "--session", path)
	if err := attestPiSessionRoute(path, argv); err != nil {
		t.Fatal(err)
	}

	checkMismatch := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || !errors.Is(err, ErrPiSessionRouteMismatch) {
			t.Fatalf("error=%v want %v", err, ErrPiSessionRouteMismatch)
		}
	}

	t.Run("different absolute path", func(t *testing.T) {
		other := writePiSession(t, root, "other.jsonl", piSessionBody("openai-codex", "gpt-5.6-luna", "medium"), 0o600)
		bad := append([]string(nil), argv...)
		bad[6] = other
		checkMismatch(t, attestPiSessionRoute(path, bad))
	})
	t.Run("relative suffix", func(t *testing.T) {
		bad := append([]string(nil), argv...)
		bad[6] = "relative.jsonl"
		checkMismatch(t, attestPiSessionRoute(path, bad))
	})
	t.Run("wrong suffix flag", func(t *testing.T) {
		bad := append([]string(nil), argv...)
		bad[5] = "--sessions"
		checkMismatch(t, attestPiSessionRoute(path, bad))
	})
}

func TestCreatePiLaunchSession(t *testing.T) {
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role:              router.RoleWorker,
		Shape:             launch.Implementation,
		RequestedProvider: launch.WorkerProvider,
		RequestedModel:    launch.WorkerModel,
		RequestedEffort:   launch.WorkerEffort,
		TaskRef:           "FAC-188",
		LeaseGeneration:   7,
		Scope:             router.ScopeTask,
		ProbeResults:      map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := configurePiSessionRoot(t)
	req := launch.Request{
		Decision:          d,
		TaskRef:           "FAC-188",
		Repository:        "repo-id",
		Lane:              "worker",
		SessionGeneration: 42,
		Scope:             router.ScopeTask,
		LeaseGeneration:   d.LeaseGeneration,
	}
	path, err := createPiLaunchSession(req)
	if err != nil {
		t.Fatal(err)
	}
	herdDir := filepath.Join(root, "herdforge")
	herdDir, err = filepath.EvalSymlinks(herdDir)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(herdDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Fatalf("path %q escapes %q: rel=%q err=%v", path, herdDir, rel, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("mode=%v want regular non-symlink file", info.Mode())
	}
	if info.Size() != 0 {
		t.Fatalf("size=%d want 0", info.Size())
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm=%04o want 0600", info.Mode().Perm())
	}
	if _, err := createPiLaunchSession(req); err == nil {
		t.Fatal("second identical allocation must fail exclusively")
	}
}
