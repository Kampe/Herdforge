package quotasup

import (
	"fmt"
	"os"
	"testing"

	"github.com/Kampe/Herdforge/pkg/laneenv"
)

func TestMain(m *testing.M) {
	laneenv.Strip()
	restore, err := laneenv.IsolateDefaultSlotDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolate test heavy-phase slots: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	restore()
	os.Exit(code)
}
