package toolchild

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The runner form must produce byte-identical bindings to the legacy rule for
// every origin shape, because there is only one normalization and both entry
// points share it. A second copy that drifted is exactly what this API exists
// to prevent.
func TestRepositoryIdentityWithRunnerNormalizesEveryOriginShape(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name, origin, want string
	}{
		{"https url", "https://github.com/Kampe/Herdforge.git", "github.com/Kampe/Herdforge"},
		{"https url without suffix", "https://GitHub.com/Kampe/Herdforge", "github.com/Kampe/Herdforge"},
		{"ssh url", "ssh://git@github.com/Kampe/Herdforge.git", "github.com/Kampe/Herdforge"},
		{"scp style", "git@github.com:Kampe/Herdforge.git", "github.com/Kampe/Herdforge"},
		{"scp style uppercase host", "git@GitHub.com:Kampe/Herdforge", "github.com/Kampe/Herdforge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RepositoryIdentityWithRunner(root, func(...string) (string, error) {
				return tc.origin + "\n", nil
			})
			if err != nil {
				t.Fatalf("origin %q: %v", tc.origin, err)
			}
			if got != tc.want {
				t.Fatalf("origin %q gave %q, want %q", tc.origin, got, tc.want)
			}
		})
	}
}

// Local origins, asserted per SHAPE, because the legacy contract treats them
// differently and the difference is easy to get wrong.
//
// CI 34743907915 caught my first version of this test asserting one result for
// all three shapes. It is wrong: "file://" contains "://", so a file URL never
// reaches the local-path branch at all. url.Parse gives it an EMPTY host, the
// scheme branch declines, and the SCP branch then takes it with host "file" and
// path "//abs", trimmed to "abs" — yielding "file/abs" with ONE slash. A bare
// absolute path has no colon, so it does reach the local branch and yields
// "file/" + Clean(abs), which has TWO slashes.
//
// That asymmetry is pre-existing production behaviour, moved here verbatim. The
// test is corrected to the contract; the contract is NOT bent to the test. It is
// reported to root as a wart rather than changed, because an identity rule that
// shifts is worse than one that is merely odd.
func TestRepositoryIdentityWithRunnerResolvesLocalOrigins(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ name, origin, want string }{
		{"file url takes the scp branch", "file://" + root, "file/" + strings.Trim(root, "/")},
		{"bare absolute path", root, "file/" + filepath.Clean(root)},
		{"relative path resolves against root", ".", "file/" + filepath.Clean(root)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RepositoryIdentityWithRunner(root, func(...string) (string, error) { return tc.origin, nil })
			if err != nil {
				t.Fatalf("origin %q: %v", tc.origin, err)
			}
			if got != tc.want {
				t.Fatalf("origin %q gave %q, want %q", tc.origin, got, tc.want)
			}
		})
	}
}

// The legacy rule is unchanged by the runner form: both entry points must agree
// on every shape, including the odd one above.
func TestRepositoryIdentityAgreesWithTheRunnerForm(t *testing.T) {
	root := t.TempDir()
	for _, origin := range []string{"file://" + root, root, ".", "https://github.com/a/b.git", "git.com:a/b.git"} {
		want, errA := RepositoryIdentityWithRunner(root, func(...string) (string, error) { return origin, nil })
		got, errB := RepositoryIdentityWithRunner(root, func(...string) (string, error) { return origin + "\n", nil })
		if (errA == nil) != (errB == nil) {
			t.Fatalf("origin %q: trailing newline changed the outcome: %v vs %v", origin, errA, errB)
		}
		if got != want {
			t.Fatalf("origin %q: trailing newline changed the binding: %q vs %q", origin, got, want)
		}
	}
}

// CREDENTIALS must not survive into the binding, and must not be echoed by any
// error this function returns.
func TestRepositoryIdentityWithRunnerDropsCredentialMaterial(t *testing.T) {
	root := t.TempDir()
	const secret = "s3cr3t-token"
	for _, origin := range []string{
		"https://user:" + secret + "@github.com/Kampe/Herdforge.git",
		"ssh://" + secret + "@github.com/Kampe/Herdforge.git",
		secret + "@github.com:Kampe/Herdforge.git",
	} {
		got, err := RepositoryIdentityWithRunner(root, func(...string) (string, error) { return origin, nil })
		if err != nil {
			t.Fatalf("origin: %v", err)
		}
		if strings.Contains(got, secret) {
			t.Fatalf("the binding carries credential material: %q", got)
		}
		if got != "github.com/Kampe/Herdforge" {
			t.Fatalf("binding = %q, want the host/path form", got)
		}
	}
}

// The exact argument contract: what the leaf asks for, and what it must NOT.
func TestRepositoryIdentityWithRunnerUsesTheExactArgumentContract(t *testing.T) {
	var seen []string
	if _, err := RepositoryIdentityWithRunner(t.TempDir(), func(args ...string) (string, error) {
		seen = args
		return "https://github.com/a/b.git", nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"config", "--get", "remote.origin.url"}
	if len(seen) != len(want) {
		t.Fatalf("argv = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("argv = %v, want %v", seen, want)
		}
	}
	for _, a := range seen {
		if a == "-C" {
			t.Fatal("the leaf passed -C; the runner owns the repository directory, and two places choosing it can disagree")
		}
	}
}

// A runner error must stay recognisable, or a caller's budget or cancellation
// refusal would read as an ordinary identity failure.
func TestRepositoryIdentityWithRunnerPreservesRunnerErrors(t *testing.T) {
	sentinel := errors.New("caller budget refused")
	_, err := RepositoryIdentityWithRunner(t.TempDir(), func(...string) (string, error) { return "", sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the runner's own error to survive errors.Is", err)
	}
}

// Refusals happen before anything runs.
func TestRepositoryIdentityWithRunnerRefusesNilRunnerAndEmptyRoot(t *testing.T) {
	called := false
	run := func(...string) (string, error) { called = true; return "", nil }
	if _, err := RepositoryIdentityWithRunner("  ", run); !errors.Is(err, ErrUnsafeTeardown) {
		t.Fatalf("empty root err = %v, want ErrUnsafeTeardown", err)
	}
	if _, err := RepositoryIdentityWithRunner(t.TempDir(), nil); !errors.Is(err, ErrNilIdentityRunner) {
		t.Fatalf("nil runner err = %v, want ErrNilIdentityRunner", err)
	}
	if called {
		t.Fatal("a refused call still ran a command")
	}
}

// An empty origin is still a refusal, not an empty binding.
func TestRepositoryIdentityWithRunnerRefusesAnEmptyOrigin(t *testing.T) {
	if _, err := RepositoryIdentityWithRunner(t.TempDir(), func(...string) (string, error) { return "  \n", nil }); err == nil {
		t.Fatal("an empty origin produced a binding")
	}
}
