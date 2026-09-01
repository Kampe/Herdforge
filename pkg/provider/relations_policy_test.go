package provider

import "testing"

type taskProviderWithoutTraversalPolicy struct {
	TaskProvider
}

func TestResolveRelationTraversalConcurrency(t *testing.T) {
	tests := []struct {
		name string
		tp   TaskProvider
		want int
	}{
		{name: "nil provider is serial", want: 1},
		{name: "unknown provider is serial", tp: taskProviderWithoutTraversalPolicy{TaskProvider: NewMemoryProvider()}, want: 1},
		{name: "memory provider declares bounded in-process policy", tp: NewMemoryProvider(), want: DefaultBulkRelationConcurrency},
		{name: "linear configured policy", tp: &LinearProvider{BulkConcurrency: 3}, want: 3},
		{name: "bound client delegates policy", tp: NewBoundClient(&LinearProvider{BulkConcurrency: 4}, DefaultDeadlines()), want: 4},
		{name: "kaneo policy is capped", tp: &KaneoProvider{BulkConcurrency: MaxRelationTraversalConcurrency + 1}, want: MaxRelationTraversalConcurrency},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveRelationTraversalConcurrency(tt.tp); got != tt.want {
				t.Fatalf("ResolveRelationTraversalConcurrency(%T) = %d, want %d", tt.tp, got, tt.want)
			}
		})
	}
}
