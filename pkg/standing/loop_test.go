package standing

import "testing"

func TestLoopRegistryTransitions(t *testing.T) {
	tests := []struct {
		name string
		mode LoopMode
		want PromptDisposition
	}{
		{name: "running prompt remains standing", mode: LoopRunning, want: PromptStanding},
		{name: "held prompt is one-shot", mode: LoopHeld, want: PromptOneShot},
		{name: "one-shot does not become indefinite", mode: LoopOneShot, want: PromptOneShot},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewLoopRegistry([]LoopState{{Lane: "smith", Mode: tc.mode, Goal: "goal", Wakeup: "wakeup"}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.Prompt("smith")
			if err != nil || got != tc.want {
				t.Fatalf("Prompt() = %q, %v; want %q", got, err, tc.want)
			}
			state, err := r.State("smith")
			if err != nil {
				t.Fatal(err)
			}
			if tc.mode != LoopRunning && state.Mode != LoopOneShot {
				t.Fatalf("held prompt mode = %q, want one-shot", state.Mode)
			}
		})
	}
}

func TestLoopRegistryHoldAndAtomicRelease(t *testing.T) {
	r, err := NewLoopRegistry([]LoopState{{Lane: "smith", Mode: LoopRunning, Goal: "goal", Wakeup: "wakeup"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Hold("smith"); err != nil {
		t.Fatal(err)
	}
	state, err := r.State("smith")
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != LoopHeld || state.Goal != "" || state.Wakeup != "" {
		t.Fatalf("held state = %+v, want cleared held state", state)
	}
	if err := r.Release("smith", "", "wakeup"); err == nil {
		t.Fatal("incomplete release unexpectedly succeeded")
	}
	state, err = r.State("smith")
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != LoopHeld || state.Goal != "" || state.Wakeup != "" {
		t.Fatalf("failed release partially applied: %+v", state)
	}
	if err := r.Release("smith", "restored goal", "restored wakeup"); err != nil {
		t.Fatal(err)
	}
	state, err = r.State("smith")
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != LoopRunning || state.Goal != "restored goal" || state.Wakeup != "restored wakeup" {
		t.Fatalf("released state = %+v", state)
	}
}
