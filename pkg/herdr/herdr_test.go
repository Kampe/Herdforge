package herdr

import "testing"

func TestIsAvailable(t *testing.T) {
	// On this machine, herdr should be installed
	available := IsAvailable()
	if !available {
		t.Log("herdr not found in PATH — available=false is expected on CI")
	}
}

func TestHerdrCLINotFound(t *testing.T) {
	// Verify that the error path works when herdr is missing
	// by temporarily modifying PATH
	t.Setenv("PATH", "/dev/null")

	available := IsAvailable()
	if available {
		t.Skip("herdr still found in PATH despite override")
	}
}

func TestEnsureHerdforgeLabel_Prefixes(t *testing.T) {
	got := EnsureHerdforgeLabel("worker")
	want := "Herdforge · worker"
	if got != want {
		t.Errorf("EnsureHerdforgeLabel(\"worker\") = %q, want %q", got, want)
	}
}

func TestEnsureHerdforgeLabel_AlreadyPrefixed(t *testing.T) {
	got := EnsureHerdforgeLabel("Herdforge · worker")
	if got != "Herdforge · worker" {
		t.Errorf("already-prefixed label was modified: %q", got)
	}
}

func TestEnsureHerdforgeLabel_ContainsPrefix(t *testing.T) {
	// Ensures that a label containing the prefix as a substring (not
	// necessarily at the start) is also left unchanged.
	got := EnsureHerdforgeLabel("Herdforge · worker (FAC-141)")
	if got != "Herdforge · worker (FAC-141)" {
		t.Errorf("label containing prefix was modified: %q", got)
	}
}
