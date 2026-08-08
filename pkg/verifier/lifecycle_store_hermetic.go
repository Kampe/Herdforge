//go:build fac151_hermetic_integration

package verifier

import "errors"

// openLifecycleStore is deliberately unavailable inside the FAC-151
// hermetic profile. That profile compiles this package inside the
// verification container, which has no network and a module cache seeded
// with exactly one hash-pinned module, so the SQLite-backed store cannot be
// linked here (see lifecycle_store.go).
//
// Nothing inside the container runs the host-side Docker runner — only the
// FAC-151 admission tests execute there — so this path is never reached in
// practice. It fails closed rather than returning a no-op store: a silently
// degraded lifecycle store would let a container be created with no durable
// receipt, which is precisely the failure the receipt exists to prevent.
func openLifecycleStore(string) (lifecycleStore, error) {
	return nil, errors.New("container lifecycle store is unavailable in the FAC-151 hermetic profile")
}
