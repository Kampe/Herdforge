package scopefence

import "context"

// AuthorityReceipt is an opaque coordinator/root receipt. Scopefence stores
// only its public binding fields; authentication is delegated to a protected
// verifier that is injected by the coordinator. No signing key or signing API
// exists in this package.
type AuthorityReceipt struct {
	Kind          string `json:"kind"`
	Repository    string `json:"repository"`
	Task          string `json:"task,omitempty"`
	Revision      string `json:"revision"`
	Files         int    `json:"files"`
	PayloadDigest string `json:"payload_digest"`
	Opaque        []byte `json:"opaque,omitempty"`
}

// ReleaseAuthority is separate from graph/scope publication authority. A
// graph receipt can never authorize RootAdmittedMerge or FencedAbandonment.
type ReleaseAuthority interface {
	VerifyRelease(context.Context, ReleaseRequest) error
}
