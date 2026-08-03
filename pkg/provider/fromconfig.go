package provider

import (
	"context"
	"time"
)

// DeadlinesFromParts builds a Deadlines value from optional config-resolved
// durations. Zero means "package default" after Normalize.
func DeadlinesFromParts(get, list, mutate, comment, readback time.Duration) Deadlines {
	return Deadlines{
		Get:      get,
		List:     list,
		Mutate:   mutate,
		Comment:  comment,
		Readback: readback,
	}.Normalize()
}

// ApplyDeadlines sets per-op deadlines on known production provider types.
// Unknown TaskProvider implementations are left unchanged (FAC-155 may wrap).
func ApplyDeadlines(tp TaskProvider, d Deadlines) {
	d = d.Normalize()
	switch p := tp.(type) {
	case *BoundClient:
		p.Deadlines = d
		ApplyDeadlines(p.Inner, d)
	case *KaneoProvider:
		p.Deadlines = d
	case *GitHubProvider:
		p.Deadlines = d
	case *LinearProvider:
		p.Deadlines = d
	case *JiraProvider:
		p.Deadlines = d
	case *AzureDevOpsProvider:
		p.Deadlines = d
	}
}

// BoundOp derives a child context for an external provider boundary call.
// Production callers (daemon/dispatch) must use this (or equivalent) instead
// of context.Background() across TaskProvider methods.
func BoundOp(ctx context.Context, d Deadlines, op OpKind) (context.Context, context.CancelFunc) {
	return WithOpDeadline(ctx, d, op)
}
