package provider

import (
	"fmt"
	"time"
)

// TaskConfig is the production config surface for building a board provider.
// Mirrors config.TaskProvider fields used at activation (FAC-150).
// Non-Kaneo live activation is intentionally not expanded here (FAC-155).
type TaskConfig struct {
	Type      string
	APIURL    string
	ProjectID string
	UseCLI    bool
	// Optional resolved deadline parts (0 = package default).
	Get, List, Mutate, Comment, Readback time.Duration
}

// NewProductionProvider builds the live TaskProvider for herd/daemon/dispatch.
// Kaneo is the only configured production board type; other types return an
// error so FAC-155 can own activation. Callers that need in-process tests use
// NewMemoryProvider / NewBoundClient directly.
func NewProductionProvider(tc TaskConfig) (TaskProvider, error) {
	dls := DeadlinesFromParts(tc.Get, tc.List, tc.Mutate, tc.Comment, tc.Readback)
	switch tc.Type {
	case "kaneo":
		k := NewKaneoProvider(tc.APIURL, tc.ProjectID, tc.UseCLI)
		ApplyDeadlines(k, dls)
		return NewBoundClient(k, dls), nil
	case "memory":
		// Explicit test/dev type — still bound so timeouts classify uniformly.
		return NewBoundClient(NewMemoryProvider(), dls), nil
	case "":
		return nil, fmt.Errorf("task_provider.type is required")
	default:
		// FAC-155 owns non-Kaneo activation; refuse silent foreign dial.
		return nil, fmt.Errorf("task_provider.type %q is not activated in this build (FAC-155; live board is Kaneo only)", tc.Type)
	}
}

// MustProductionProvider is for tests; panics on error.
func MustProductionProvider(tc TaskConfig) TaskProvider {
	tp, err := NewProductionProvider(tc)
	if err != nil {
		panic(err)
	}
	return tp
}
