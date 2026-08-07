package provider

import (
	"context"
	"strings"
)

type casCtxKey int

const (
	ctxFenceToken casCtxKey = iota + 1
	ctxOpID
	ctxExpectedStatus
	ctxExpectedComment
	ctxExclusiveTask // taskID already under FenceStore.WithExclusive
	ctxMintIdentity  // immutable per-call lease identity for coordinator mint
)

// MintIdentity is immutable per-call lease binding for coordinator capability mint.
// Never stored on shared KaneoProvider fields (avoids concurrent cross-wire).
type MintIdentity struct {
	Repo, Provider, Project, TaskRef, OwnerID string
}

// WithMintIdentity attaches immutable mint lease identity for one mutation call.
func WithMintIdentity(ctx context.Context, id MintIdentity) context.Context {
	return context.WithValue(ctx, ctxMintIdentity, id)
}

// MintIdentityFrom returns per-call mint identity if present.
func MintIdentityFrom(ctx context.Context) (MintIdentity, bool) {
	v, ok := ctx.Value(ctxMintIdentity).(MintIdentity)
	return v, ok && v.OwnerID != "" && v.TaskRef != ""
}

// withExclusiveHeld marks that taskID's exclusive section is already held
// so AuthBroker.Execute does not re-enter WithExclusive (deadlock).
func withExclusiveHeld(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, ctxExclusiveTask, taskID)
}

func exclusiveHeldTask(ctx context.Context) string {
	v, _ := ctx.Value(ctxExclusiveTask).(string)
	return v
}

// WithCASMeta attaches fence token + opID for TaskProvider HTTP transport
// (Kaneo X-Herd-Fence / X-Herd-Op). FencedCAS.CompareAndSwap injects these
// into the mutate context so the authoritative service can enforce them.
func WithCASMeta(ctx context.Context, fenceToken int64, opID string) context.Context {
	ctx = context.WithValue(ctx, ctxFenceToken, fenceToken)
	ctx = context.WithValue(ctx, ctxOpID, opID)
	return ctx
}

// WithCASExpectation records what the mutation must achieve for ambiguous
// reconciliation (status and/or comment body).
func WithCASExpectation(ctx context.Context, status, commentBody string) context.Context {
	if status != "" {
		ctx = context.WithValue(ctx, ctxExpectedStatus, NormalizeStatus(status))
	}
	if commentBody != "" {
		ctx = context.WithValue(ctx, ctxExpectedComment, commentBody)
	}
	return ctx
}

func casFenceToken(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(ctxFenceToken).(int64)
	return v, ok
}

func casOpID(ctx context.Context) string {
	v, _ := ctx.Value(ctxOpID).(string)
	return v
}

func casExpectedStatus(ctx context.Context) string {
	v, _ := ctx.Value(ctxExpectedStatus).(string)
	return v
}

func casExpectedComment(ctx context.Context) string {
	v, _ := ctx.Value(ctxExpectedComment).(string)
	return v
}

// AttachFenceHeaders sets X-Herd-Fence / X-Herd-Op when CAS meta is present.
// Production Kaneo HTTP and CLI-compatible sandboxes must honor these.
func AttachFenceHeaders(ctx context.Context, set func(key, value string)) {
	if op := casOpID(ctx); op != "" {
		set("X-Herd-Op", op)
	}
	if tok, ok := casFenceToken(ctx); ok {
		set("X-Herd-Fence", fmtInt64(tok))
	}
}

// CommentOpTag is the live-board marker binding a comment body to one opID.
// Exactly-once comment identity: two ops with the same body remain distinct.
const CommentOpTagPrefix = "[herd-op:"

// CommentOpTaggedBody appends an op-bound marker so live ListLiveComments
// can prove this exact operation (not a prior substring-matching comment).
func CommentOpTaggedBody(body, opID string) string {
	if opID == "" {
		return body
	}
	marker := CommentOpTagPrefix + opID + "]"
	if strings.Contains(body, marker) {
		return body
	}
	if body == "" {
		return marker
	}
	return body + "\n" + marker
}

// MatchCommentOp reports whether liveBody is the op-bound comment for
// wantBody+opID. When opID is set, the [herd-op:id] marker is required —
// bare free-text equality is never sufficient (prevents cross-op collapse).
func MatchCommentOp(liveBody, wantBody, opID string) bool {
	if opID == "" {
		return liveBody == wantBody
	}
	marker := CommentOpTagPrefix + opID + "]"
	if !strings.Contains(liveBody, marker) {
		return false
	}
	tagged := CommentOpTaggedBody(wantBody, opID)
	if liveBody == tagged {
		return true
	}
	// wantBody may already include the marker (full expected comment).
	if liveBody == wantBody && strings.Contains(wantBody, marker) {
		return true
	}
	// Prefix form: free text then marker line.
	return liveBody == wantBody+"\n"+marker || strings.HasPrefix(liveBody, wantBody+"\n"+CommentOpTagPrefix)
}

func fmtInt64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
