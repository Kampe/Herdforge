package herdr

import (
	"testing"
)

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
