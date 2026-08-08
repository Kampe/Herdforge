package coordinator

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegisterWritesDurableIdentity(t *testing.T) {
	dir := t.TempDir()
	reg, err := Register(dir, "", "wK")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.Name != CoordinatorName {
		t.Fatalf("name = %q, want %q", reg.Name, CoordinatorName)
	}
	if reg.Workspace != "wK" {
		t.Fatalf("workspace = %q, want wK", reg.Workspace)
	}
	if reg.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d", reg.PID, os.Getpid())
	}
	if reg.StartedAt.IsZero() {
		t.Fatal("started_at must be set")
	}

	path := filepath.Join(dir, RegistrationFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("registration file not written: %v", err)
	}
}

func TestResolveReadsBackRegistration(t *testing.T) {
	dir := t.TempDir()
	if _, err := Register(dir, "", "wF"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if reg.Name != CoordinatorName {
		t.Fatalf("name = %q, want %q", reg.Name, CoordinatorName)
	}
	if reg.Workspace != "wF" {
		t.Fatalf("workspace = %q, want wF", reg.Workspace)
	}
}

func TestResolveAbsentFileReturnsDefaultName(t *testing.T) {
	dir := t.TempDir()
	reg, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve on absent file must not error: %v", err)
	}
	if reg.Name != CoordinatorName {
		t.Fatalf("absent file name = %q, want default %q", reg.Name, CoordinatorName)
	}
	if reg.Workspace != "" {
		t.Fatalf("absent file workspace = %q, want empty", reg.Workspace)
	}
}

func TestResolveCorruptFileIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, RegistrationFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(dir); err == nil {
		t.Fatal("corrupt registration must be an error, not silently discarded")
	}
}

func TestRegisterOverwritesStaleRecord(t *testing.T) {
	dir := t.TempDir()
	first, err := Register(dir, "", "wK")
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}
	time.Sleep(time.Millisecond)
	second, err := Register(dir, "", "wK")
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if !second.StartedAt.After(first.StartedAt) {
		t.Fatal("second registration must supersede the stale one")
	}
	reg, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reg.StartedAt.Equal(second.StartedAt) {
		t.Fatal("Resolve must read the latest registration, not the stale one")
	}
}

func TestRegisterEmptyRootUsesCwd(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Register("", "", "wK"); err != nil {
		t.Fatalf("Register with empty root: %v", err)
	}
	if _, err := os.Stat(RegistrationFile); err != nil {
		t.Fatalf("registration file not written in cwd: %v", err)
	}
}

func TestRegisterCustomNameFlowsThroughResolve(t *testing.T) {
	dir := t.TempDir()
	custom := "forge-coordinator"
	reg, err := Register(dir, custom, "wK")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.Name != custom {
		t.Fatalf("Register name = %q, want %q", reg.Name, custom)
	}
	resolved, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Name != custom {
		t.Fatalf("Resolve name = %q, want %q", resolved.Name, custom)
	}
}
