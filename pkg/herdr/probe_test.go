package herdr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// withStubOpencode puts a fake `opencode` on PATH whose behavior per model is
// scripted, so ProbeModel/ResolveHealthyModel are hermetic.
func withStubOpencode(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	stub := "#!/bin/sh\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestProbeModel_Healthy(t *testing.T) {
	withStubOpencode(t, `echo "PROBE_OK"`)
	r := ProbeModel(context.Background(), "litellm/lazer/deepseek-v4-flash")
	if !r.Available {
		t.Fatalf("healthy model must probe available: %+v", r)
	}
}

func TestProbeModel_Exhausted(t *testing.T) {
	withStubOpencode(t, `echo "No payment method. Add one at ..."; exit 1`)
	r := ProbeModel(context.Background(), "opencode/deepseek-v4-flash-free")
	if r.Available || r.Reason != "no payment method" {
		t.Fatalf("exhausted model must be caught: %+v", r)
	}
}

func TestProbeModel_QuotaSignal(t *testing.T) {
	withStubOpencode(t, `echo "error: quota exceeded for this key"; exit 1`)
	r := ProbeModel(context.Background(), "m")
	if r.Available || r.Reason != "quota" {
		t.Fatalf("quota signal must be caught: %+v", r)
	}
}

func TestResolveHealthyModel_FailsOver(t *testing.T) {
	// Primary emits an exhaustion signal; the fallback echoes PROBE_OK. The
	// stub distinguishes by the --model arg.
	withStubOpencode(t, `
for a in "$@"; do
  case "$a" in
    *free) echo "No payment method"; exit 1 ;;
    *lazer*) echo "PROBE_OK"; exit 0 ;;
  esac
done
echo "PROBE_OK"`)
	got, trail := ResolveHealthyModel(context.Background(),
		"opencode/deepseek-v4-flash-free",
		[]string{"litellm/lazer/deepseek-v4-flash"})
	if got != "litellm/lazer/deepseek-v4-flash" {
		t.Fatalf("must fail over to healthy fallback, got %q (trail %+v)", got, trail)
	}
	if len(trail) != 2 || trail[0].Available || !trail[1].Available {
		t.Fatalf("trail must show primary-exhausted then fallback-ok: %+v", trail)
	}
}

func TestResolveHealthyModel_AllExhausted(t *testing.T) {
	withStubOpencode(t, `echo "quota exceeded"; exit 1`)
	got, trail := ResolveHealthyModel(context.Background(), "a", []string{"b"})
	if got != "" {
		t.Fatalf("all-exhausted must return empty, got %q", got)
	}
	if len(trail) != 2 {
		t.Fatalf("trail must probe every candidate: %+v", trail)
	}
}
