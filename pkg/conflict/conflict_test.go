package conflict

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseConflictMarkersAndResolve_FailsClosed(t *testing.T) {
	input := `func main() {
<<<<<<< HEAD
	fmt.Println("Hello from ours")
=======
	fmt.Println("Hello from theirs")
>>>>>>> feature-branch
}`

	chunks, hasConflicts := ParseConflictMarkers(input)
	if !hasConflicts || len(chunks) != 1 {
		t.Fatalf("expected 1 conflict chunk, got %d", len(chunks))
	}

	if !strings.Contains(chunks[0].Ours, "ours") || !strings.Contains(chunks[0].Theirs, "theirs") {
		t.Errorf("unexpected conflict chunk content: %+v", chunks[0])
	}

	resolver := NewResolver("gpt-4o")
	resolved, err := resolver.ResolveFile(context.Background(), input)
	if err == nil {
		t.Fatalf("expected fail-closed resolution error, got success with output: %s", resolved)
	}

	if !errors.Is(err, ErrUnresolved) {
		t.Errorf("expected ErrUnresolved, got: %v", err)
	}

	var unresolvedErr *UnresolvedConflictError
	if !errors.As(err, &unresolvedErr) {
		t.Errorf("expected *UnresolvedConflictError, got %T", err)
	} else if len(unresolvedErr.Chunks) != 1 {
		t.Errorf("expected 1 unresolved chunk in error, got %d", len(unresolvedErr.Chunks))
	}

	// Must never concatenate ours and theirs
	if strings.Contains(resolved, "Hello from ours") && strings.Contains(resolved, "Hello from theirs") {
		t.Errorf("forbidden concatenation of ours and theirs detected in output")
	}
}

func TestResolveFile_IdenticalChunks(t *testing.T) {
	input := `package main

func main() {
<<<<<<< HEAD
	fmt.Println("Identical change")
=======
	fmt.Println("Identical change")
>>>>>>> feature-branch
}
`
	resolver := NewResolver("manual")
	resolved, err := resolver.ResolveFile(context.Background(), input)
	if err != nil {
		t.Fatalf("expected clean resolution for identical chunks, got err: %v", err)
	}

	expected := `package main

func main() {
	fmt.Println("Identical change")
}
`
	if resolved != expected {
		t.Errorf("expected %q, got %q", expected, resolved)
	}
}

func TestResolveFile_Strategies(t *testing.T) {
	input := `func foo() {
<<<<<<< HEAD
	ours()
||||||| ancestor
	base()
=======
	theirs()
>>>>>>> branch
}
`

	t.Run("strategy ours", func(t *testing.T) {
		res := NewResolverWithStrategy(StrategyOurs)
		out, err := res.ResolveFile(context.Background(), input)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !strings.Contains(out, "ours()") || strings.Contains(out, "theirs()") || strings.Contains(out, "base()") {
			t.Errorf("unexpected output: %s", out)
		}
	})

	t.Run("strategy theirs", func(t *testing.T) {
		res := NewResolverWithStrategy(StrategyTheirs)
		out, err := res.ResolveFile(context.Background(), input)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !strings.Contains(out, "theirs()") || strings.Contains(out, "ours()") || strings.Contains(out, "base()") {
			t.Errorf("unexpected output: %s", out)
		}
	})

	t.Run("strategy base", func(t *testing.T) {
		res := NewResolverWithStrategy(StrategyBase)
		out, err := res.ResolveFile(context.Background(), input)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !strings.Contains(out, "base()") || strings.Contains(out, "ours()") || strings.Contains(out, "theirs()") {
			t.Errorf("unexpected output: %s", out)
		}
	})

	t.Run("strategy base without diff3 fails", func(t *testing.T) {
		noBaseInput := `<<<<<<< HEAD
ours
=======
theirs
>>>>>>> branch
`
		res := NewResolverWithStrategy(StrategyBase)
		_, err := res.ResolveFile(context.Background(), noBaseInput)
		if err == nil || !errors.Is(err, ErrUnresolved) {
			t.Fatalf("expected ErrUnresolved for missing base, got: %v", err)
		}
	})
}

func TestResolveFile_CustomResolveFn(t *testing.T) {
	input := `start
<<<<<<< HEAD
old
=======
new
>>>>>>> branch
end
`
	res := NewResolverWithFunc(func(ctx context.Context, chunk ConflictChunk) (string, error) {
		return "resolved_custom\n", nil
	})

	out, err := res.ResolveFile(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	expected := "start\nresolved_custom\nend\n"
	if out != expected {
		t.Errorf("expected %q, got %q", expected, out)
	}
}

func TestResolveFile_CustomResolveFn_ReturnsMarkers_FailsClosed(t *testing.T) {
	input := `start
<<<<<<< HEAD
old
=======
new
>>>>>>> branch
end
`
	res := NewResolverWithFunc(func(ctx context.Context, chunk ConflictChunk) (string, error) {
		return "<<<<<<< HEAD\nstill broken\n>>>>>>>\n", nil
	})

	_, err := res.ResolveFile(context.Background(), input)
	if err == nil || !errors.Is(err, ErrUnresolved) {
		t.Fatalf("expected ErrUnresolved when resolver returns markers, got: %v", err)
	}
}

func TestResolveFile_CustomResolveFn_Error(t *testing.T) {
	input := `start
<<<<<<< HEAD
old
=======
new
>>>>>>> branch
end
`
	customErr := errors.New("llm provider timeout")
	res := NewResolverWithFunc(func(ctx context.Context, chunk ConflictChunk) (string, error) {
		return "", customErr
	})

	_, err := res.ResolveFile(context.Background(), input)
	if err == nil || !errors.Is(err, customErr) {
		t.Fatalf("expected custom error to be wrapped, got: %v", err)
	}
}

func TestResolveFile_MaxChunks(t *testing.T) {
	input := `<<<<<<< HEAD
1
=======
2
>>>>>>> a
<<<<<<< HEAD
3
=======
4
>>>>>>> b
`
	res := NewResolverWithStrategy(StrategyOurs)
	res.MaxChunks = 1

	_, err := res.ResolveFile(context.Background(), input)
	if err == nil || !errors.Is(err, ErrUnresolved) {
		t.Fatalf("expected ErrUnresolved when MaxChunks exceeded, got: %v", err)
	}
}

func TestResolveFile_NoConflicts(t *testing.T) {
	testCases := []string{
		"package main\n\nfunc main() {}\n",
		"single line no newline",
		"line 1\r\nline 2\r\n",
		"",
	}

	resolver := NewResolver("gpt-4o")
	for _, input := range testCases {
		out, err := resolver.ResolveFile(context.Background(), input)
		if err != nil {
			t.Fatalf("expected nil err for no conflicts, got: %v", err)
		}
		if out != input {
			t.Errorf("expected exact match: got %q, want %q", out, input)
		}
	}
}

func TestResolveFile_MalformedMarkers(t *testing.T) {
	malformedInputs := []struct {
		name  string
		input string
	}{
		{
			name: "unterminated conflict at EOF",
			input: `func foo() {
<<<<<<< HEAD
	fmt.Println("ours")
=======
	fmt.Println("theirs")
`,
		},
		{
			name: "stray separator outside conflict",
			input: `func foo() {
=======
}
`,
		},
		{
			name: "stray closing marker outside conflict",
			input: `func foo() {
>>>>>>> branch
}
`,
		},
		{
			name: "stray base marker outside conflict",
			input: `func foo() {
||||||| ancestor
}
`,
		},
		{
			name: "closing before separator",
			input: `func foo() {
<<<<<<< HEAD
	ours()
>>>>>>> branch
}
`,
		},
		{
			name: "nested opening marker",
			input: `func foo() {
<<<<<<< HEAD
<<<<<<< HEAD
	ours()
=======
	theirs()
>>>>>>> branch
}
`,
		},
		{
			name: "duplicate separator marker",
			input: `func foo() {
<<<<<<< HEAD
	ours()
=======
	middle()
=======
	theirs()
>>>>>>> branch
}
`,
		},
	}

	resolver := NewResolverWithStrategy(StrategyOurs)
	for _, tc := range malformedInputs {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolver.ResolveFile(context.Background(), tc.input)
			if err == nil {
				t.Fatalf("expected error on malformed markers, got nil")
			}
			if !errors.Is(err, ErrMalformedMarkers) {
				t.Errorf("expected ErrMalformedMarkers, got: %v", err)
			}
			var malformedErr *MalformedMarkerError
			if !errors.As(err, &malformedErr) {
				t.Errorf("expected *MalformedMarkerError, got: %T", err)
			}
		})
	}
}

func TestResolveFile_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resolver := NewResolverWithStrategy(StrategyOurs)
	_, err := resolver.ResolveFile(ctx, "content")
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error, got: %v", err)
	}
}

func TestParse_Malformed(t *testing.T) {
	input := `<<<<<<< HEAD
incomplete
`
	_, err := Parse(input)
	if err == nil || !errors.Is(err, ErrMalformedMarkers) {
		t.Fatalf("expected ErrMalformedMarkers, got: %v", err)
	}
}

func TestErrorFormattingAndUnwrap(t *testing.T) {
	unresReason := &UnresolvedConflictError{Reason: "some reason"}
	if !strings.Contains(unresReason.Error(), "some reason") {
		t.Errorf("unexpected error string: %s", unresReason.Error())
	}
	if !errors.Is(unresReason, ErrUnresolved) {
		t.Errorf("expected Is(ErrUnresolved)")
	}

	wrappedErr := errors.New("underlying failure")
	unresWrapped := &UnresolvedConflictError{Err: wrappedErr}
	if !errors.Is(unresWrapped, wrappedErr) {
		t.Errorf("expected unwrap to match wrappedErr")
	}

	unresChunks := &UnresolvedConflictError{Chunks: []ConflictChunk{{Ours: "a", Theirs: "b"}}}
	if !strings.Contains(unresChunks.Error(), "1 chunk(s)") {
		t.Errorf("unexpected error string: %s", unresChunks.Error())
	}

	unresEmpty := &UnresolvedConflictError{}
	if unresEmpty.Error() != "conflict unresolved" {
		t.Errorf("unexpected empty error string: %s", unresEmpty.Error())
	}

	malformedWithLine := &MalformedMarkerError{Line: 42, Reason: "bad marker"}
	if !strings.Contains(malformedWithLine.Error(), "line 42") {
		t.Errorf("unexpected error string: %s", malformedWithLine.Error())
	}
	if !errors.Is(malformedWithLine, ErrMalformedMarkers) {
		t.Errorf("expected Is(ErrMalformedMarkers)")
	}
	if malformedWithLine.Unwrap() != ErrMalformedMarkers {
		t.Errorf("expected Unwrap to return ErrMalformedMarkers")
	}

	malformedNoLine := &MalformedMarkerError{Reason: "bad marker"}
	if malformedNoLine.Error() != "malformed conflict marker: bad marker" {
		t.Errorf("unexpected error string: %s", malformedNoLine.Error())
	}
}

func TestParseConflictMarkers_Fallback(t *testing.T) {
	// Malformed input causes Parse to fail and fallbackParse to be called
	malformed := `<<<<<<< HEAD
ours
=======
theirs
>>>>>>> branch
<<<<<<< HEAD
unterminated`

	chunks, hasConflicts := ParseConflictMarkers(malformed)
	if !hasConflicts || len(chunks) != 1 {
		t.Errorf("expected fallback to extract 1 completed chunk, got %d (hasConflicts=%v)", len(chunks), hasConflicts)
	}
}

func TestResolveFile_UnsupportedStrategy(t *testing.T) {
	input := `<<<<<<< HEAD
a
=======
b
>>>>>>> branch
`
	res := NewResolverWithStrategy(Strategy("unknown-strategy"))
	_, err := res.ResolveFile(context.Background(), input)
	if err == nil || !errors.Is(err, ErrUnresolved) {
		t.Fatalf("expected ErrUnresolved for unsupported strategy, got: %v", err)
	}
}

func TestResolveFile_LineEndingPreservation(t *testing.T) {
	// Without trailing newline
	inputNoTrailing := "package main\n<<<<<<<\na\n=======\na\n>>>>>>>"
	res := NewResolver("manual")
	out, err := res.ResolveFile(context.Background(), inputNoTrailing)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if strings.HasSuffix(out, "\n") {
		t.Errorf("expected no trailing newline, got %q", out)
	}
}

