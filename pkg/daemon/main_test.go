package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/Kampe/Herdforge/pkg/laneenv"
)

func TestMain(m *testing.M) {
	laneenv.Strip()
	restore, err := laneenv.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolate test heavy-phase slots: %v\n", err)
		os.Exit(1)
	}
	previousHermetic, hadHermetic := os.LookupEnv("HERD_HERMETIC_CONTAINER")
	if err := os.Setenv("HERD_HERMETIC_CONTAINER", "1"); err != nil {
		fmt.Fprintf(os.Stderr, "set hermetic test boundary: %v\n", err)
		restore()
		os.Exit(1)
	}
	code := m.Run()
	if hadHermetic {
		_ = os.Setenv("HERD_HERMETIC_CONTAINER", previousHermetic)
	} else {
		_ = os.Unsetenv("HERD_HERMETIC_CONTAINER")
	}
	restore()
	os.Exit(code)
}
