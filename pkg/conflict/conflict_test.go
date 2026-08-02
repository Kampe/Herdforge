package conflict

import (
	"context"
	"strings"
	"testing"
)

func TestParseConflictMarkersAndResolve(t *testing.T) {
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
	if err != nil {
		t.Fatalf("expected clean resolution, got err: %v", err)
	}

	if strings.Contains(resolved, "<<<<<<<") || strings.Contains(resolved, ">>>>>>>") {
		t.Errorf("resolved content still contains conflict markers: %s", resolved)
	}
}

func TestResolveFile_NoConflicts(t *testing.T) {
	input := "package main\n\nfunc main() {}\n"
	resolver := NewResolver("gpt-4o")
	out, err := resolver.ResolveFile(context.Background(), input)
	if err != nil || out != input {
		t.Errorf("expected untouched output for file without conflicts")
	}
}
