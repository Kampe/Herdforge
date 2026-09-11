package resolve

import (
	"fmt"
	"os"
	"testing"

	"github.com/Kampe/Herdforge/pkg/laneenv"
)

// Resolve consults pkg/posture.Effective, whose durable state falls back to the
// operator's $HOME. Without this the whole fixture suite inherits whatever
// `herd posture` the developer last set.
func TestMain(m *testing.M) {
	laneenv.Strip()
	restore, err := laneenv.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolate resolve test state: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	restore()
	os.Exit(code)
}
