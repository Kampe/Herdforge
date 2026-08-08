package verifier

import "context"

// lifecycleStore is the narrow container-lifecycle surface the hermetic
// Docker runner needs. It exists as an interface for one hard reason, not
// for generality: the FAC-151 hermetic profile compiles this whole package
// INSIDE the verification container with `go test -c -tags
// fac151_hermetic_integration ./pkg/verifier`, and that container runs with
// --network none and GOPROXY=off against a module cache seeded with exactly
// one hash-pinned module (golang.org/x/sys). A direct import of
// pkg/containerlifecycle pulls modernc.org/sqlite and its dependency tree
// into that compile, which cannot resolve offline and breaks the hermetic
// gate.
//
// The concrete SQLite-backed implementation therefore lives in
// lifecycle_store_real.go behind `!fac151_hermetic_integration`, and the
// hermetic build gets a fail-closed stub instead. Keeping the seam here —
// rather than widening the pinned module cache — preserves the minimality
// that makes the hermetic profile meaningful.
type lifecycleStore interface {
	// Register durably records a container before it is started. It must
	// be called immediately after create and before start.
	Register(containerID, taskRef, generation, imageDigest, cleanupOwner string) error
	// MarkStarted records the running transition after a successful start.
	MarkStarted(containerID string) error
	// EnsureCleanup drives teardown. Its return value is the sole authority
	// on whether cleanup succeeded: an independently proved absence is
	// definitive even when remove itself errored.
	EnsureCleanup(ctx context.Context, containerID, expectedTerminalState string, remove func(context.Context, string) error, absent func(context.Context, string) (bool, error)) error
	// RemovedWithProvenAbsence reports whether the receipt records a
	// removal whose absence was independently proved.
	RemovedWithProvenAbsence(containerID string) bool
	Close() error
}
