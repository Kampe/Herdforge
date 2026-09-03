package security

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/envelope"
)

func TestBootstrapOrLoadControlMACSecret_FirstUseCreates0600(t *testing.T) {
	root := t.TempDir()
	got, created, err := BootstrapOrLoadControlMACSecret(root, "", envelope.RoleCoordinator)
	if err != nil {
		t.Fatalf("first-use bootstrap: %v", err)
	}
	if !created {
		t.Fatal("empty root must create a durable secret")
	}
	if len(got) < 32 {
		t.Fatal("generated secret is shorter than 32 bytes")
	}
	path := ControlMACSecretPath(root)
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat durable path: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("durable path must not be a symlink")
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o want 0600", fi.Mode().Perm())
	}
	loaded, err := ReadControlMACSecret(root)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if loaded != got {
		t.Fatal("readback did not match the bootstrapped secret")
	}
}

func TestBootstrapOrLoadControlMACSecret_ReloadWithoutEnv(t *testing.T) {
	root := t.TempDir()
	first, _, err := BootstrapOrLoadControlMACSecret(root, "", envelope.RoleCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := BootstrapOrLoadControlMACSecret(root, "", envelope.RoleCoordinator)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if created {
		t.Fatal("second load must not recreate")
	}
	if second != first {
		t.Fatal("reload returned a different secret")
	}
}

func TestBootstrapOrLoadControlMACSecret_IndependentRootsDiverge(t *testing.T) {
	a, _, err := BootstrapOrLoadControlMACSecret(t.TempDir(), "", envelope.RoleCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := BootstrapOrLoadControlMACSecret(t.TempDir(), "", envelope.RoleCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("independent first-use bootstraps must not share a secret")
	}
}

func TestBootstrapOrLoadControlMACSecret_EnvMatchOrFailClosed(t *testing.T) {
	root := t.TempDir()
	durable, _, err := BootstrapOrLoadControlMACSecret(root, "", envelope.RoleCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	matched, created, err := BootstrapOrLoadControlMACSecret(root, durable, envelope.RoleCoordinator)
	if err != nil {
		t.Fatalf("matching env: %v", err)
	}
	if created || matched != durable {
		t.Fatal("matching env must load the durable secret")
	}
	before, err := os.ReadFile(ControlMACSecretPath(root))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = BootstrapOrLoadControlMACSecret(root, durable+"x", envelope.RoleCoordinator)
	if !errors.Is(err, ErrControlMACSecretConflict) {
		t.Fatalf("mismatch want conflict, got %v", err)
	}
	after, err := os.ReadFile(ControlMACSecretPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("mismatch must preserve the last valid secret")
	}
}

func TestBootstrapOrLoadControlMACSecret_OperatorEnvOnEmptyRoot(t *testing.T) {
	root := t.TempDir()
	want := "operator-provided-control-mac-secret"
	got, created, err := BootstrapOrLoadControlMACSecret(root, want, envelope.RoleCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	if !created || got != want {
		t.Fatal("empty root must persist the operator-provided env secret")
	}
}

func TestBootstrapOrLoadControlMACSecret_NonCoordinatorRefused(t *testing.T) {
	root := t.TempDir()
	roles := []string{"", RoleWorker, RoleReviewer, RoleBuilder, RoleForgeSmith, "auditor"}
	for _, role := range roles {
		_, _, err := BootstrapOrLoadControlMACSecret(root, "", role)
		if !errors.Is(err, ErrControlMACSecretRole) {
			t.Fatalf("role %q want role refusal, got %v", role, err)
		}
	}
	if _, err := os.Lstat(ControlMACSecretPath(root)); !os.IsNotExist(err) {
		t.Fatal("non-coordinator must not create mac.secret")
	}
}

func TestBootstrapOrLoadControlMACSecret_RefusesSymlinkBroadCorrupt(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		path := ControlMACSecretPath(root)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, []byte("not-the-mac-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, _, err := BootstrapOrLoadControlMACSecret(root, "", envelope.RoleCoordinator); !errors.Is(err, ErrControlMACSecretUnusable) {
			t.Fatalf("symlink want unusable, got %v", err)
		}
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("symlink must remain and not be replaced")
		}
	})
	t.Run("broad-mode", func(t *testing.T) {
		root := t.TempDir()
		path := ControlMACSecretPath(root)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("broad-permission-secret-value"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := BootstrapOrLoadControlMACSecret(root, "", envelope.RoleCoordinator); !errors.Is(err, ErrControlMACSecretUnusable) {
			t.Fatalf("broad mode want unusable, got %v", err)
		}
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Fatal("broad-mode file must not be rewritten")
		}
	})
	t.Run("empty", func(t *testing.T) {
		root := t.TempDir()
		path := ControlMACSecretPath(root)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := BootstrapOrLoadControlMACSecret(root, "", envelope.RoleCoordinator); !errors.Is(err, ErrControlMACSecretUnusable) {
			t.Fatalf("empty want unusable, got %v", err)
		}
	})
}

func TestBootstrapOrLoadControlMACSecret_ConcurrentCreateConverges(t *testing.T) {
	root := t.TempDir()
	const n = 8
	got := make([]string, n)
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			secret, _, err := BootstrapOrLoadControlMACSecret(root, "", envelope.RoleCoordinator)
			if err != nil {
				errCh <- err
				return
			}
			got[i] = secret
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent bootstrap: %v", err)
	}
	first := got[0]
	if first == "" {
		t.Fatal("concurrent bootstrap produced an empty secret")
	}
	for i, s := range got {
		if s != first {
			t.Fatalf("goroutine %d diverged", i)
		}
	}
	fi, err := os.Lstat(ControlMACSecretPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o want 0600", fi.Mode().Perm())
	}
}

func TestWriteControlMACSecret_RefusesConflictingMutation(t *testing.T) {
	root := t.TempDir()
	if err := WriteControlMACSecret(root, "original-durable-mac-secret"); err != nil {
		t.Fatal(err)
	}
	if err := WriteControlMACSecret(root, "replacement-durable-mac-secret"); !errors.Is(err, ErrControlMACSecretConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	got, err := ReadControlMACSecret(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != "original-durable-mac-secret" {
		t.Fatal("conflicting write must preserve the original secret")
	}
}

func TestReadControlMACSecret_RefusesSymlink(t *testing.T) {
	root := t.TempDir()
	path := ControlMACSecretPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "leaked")
	if err := os.WriteFile(target, []byte("follow-me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadControlMACSecret(root); !errors.Is(err, ErrControlMACSecretUnusable) {
		t.Fatalf("symlink read want unusable, got %v", err)
	}
}

func TestDefaultSecretDenyIncludesControlMACSecret(t *testing.T) {
	for _, name := range DefaultSecretDeny() {
		if name == "HERD_CONTROL_SECRET" {
			return
		}
	}
	t.Fatal("HERD_CONTROL_SECRET must be denied in agent/reviewer environments")
}
