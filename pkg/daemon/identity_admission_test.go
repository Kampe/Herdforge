package daemon

import (
	"errors"
	"testing"
)

func TestOwnershipClaimerRepositoryIdentityFailureFailsBeforeOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(string) (string, error)
	}{
		{name: "error", fn: func(string) (string, error) { return "", errors.New("repository authentication failed") }},
		{name: "empty", fn: func(string) (string, error) { return "", nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := daemonAuthenticatedRepositoryIdentity
			daemonAuthenticatedRepositoryIdentity = tc.fn
			defer func() { daemonAuthenticatedRepositoryIdentity = previous }()

			if _, err := (&Engine{}).ownershipClaimer(); err == nil {
				t.Fatal("repository identity failure unexpectedly opened ownership")
			}
		})
	}
}
