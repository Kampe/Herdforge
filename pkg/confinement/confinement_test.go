package confinement

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testIssuer struct{}

func (testIssuer) Issue(string, string, AuthTuple) (IssuerProof, error) {
	return IssuerProof{MAC: []byte("test-issuer-mac"), Nonce: "test-issuer-nonce"}, nil
}

func fixtureTuple() AuthTuple {
	return AuthTuple{Repository: "fixture-repo", Task: "FAC-190", LeaseID: "lease-1", Lane: "foundation", Session: "test-session", SessionGeneration: "generation-1", HerdrTab: "tab-1", HerdrPane: "pane-1", ProcessIdentity: "process-1", ArgvIdentity: "argv-1", PolicyDigest: "policy-v1", AllowedRoots: []string{"fixture-root"}}
}

func fixture(t *testing.T) (Boundary, Capability, string, string) {
	t.Helper()
	shared := t.TempDir()
	root := filepath.Join(shared, "FixtureWorktree")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, ".worktree-sentinel")
	if err := os.WriteFile(sentinel, []byte(sentinelContents), 0600); err != nil {
		t.Fatal(err)
	}
	b, cap, err := New(root, sentinel, fixtureTuple(), testIssuer{})
	if err != nil {
		t.Fatal(err)
	}
	return b, cap, shared, root
}

func TestAuthenticatedRelativeWriteSucceeds(t *testing.T) {
	b, cap, _, root := fixture(t)
	if err := os.Mkdir(filepath.Join(root, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := b.AuthorizeWrite(cap, "pkg/new.go"); err != nil {
		t.Fatalf("relative fixture write denied: %v", err)
	}
}

func TestIncidentPathsDenied(t *testing.T) {
	b, cap, shared, root := fixture(t)
	if err := os.WriteFile(filepath.Join(shared, "shared.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(shared, "SiblingWorktree"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(shared, "fixture-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(cap.root, "inside-alias")); err != nil {
		t.Fatal(err)
	}
	caseAlias := filepath.Join(shared, "fixtureworktree", "escape.txt")
	paths := map[string]string{
		"absolute shared-root write": filepath.Join(shared, "shared.txt"),
		"sibling worktree":           filepath.Join(shared, "SiblingWorktree", "x"),
		"parent traversal":           "../shared.txt",
		"symlink alias":              filepath.Join(shared, "fixture-alias", "x"),
		"in-root symlink escape":     filepath.Join(cap.root, "inside-alias", "x"),
		"existing in-root symlink":   filepath.Join(cap.root, "inside-alias"),
		"case alias":                 caseAlias,
		"hook":                       filepath.Join(shared, "hook-output"),
		"patch helper":               filepath.Join(shared, "patch-output"),
		"shell redirection":          filepath.Join(shared, "redirect-output"),
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			if err := b.AuthorizeWrite(cap, path); err == nil {
				t.Fatalf("incident path %q was authorized", path)
			}
		})
	}
}

func TestNestedRelativeWriteSucceeds(t *testing.T) {
	b, cap, _, _ := fixture(t)
	if err := b.AuthorizeWrite(cap, "new/nested/file.go"); err != nil {
		t.Fatalf("nested relative write denied: %v", err)
	}
}

func TestChildAndGrandchildAttemptsDenied(t *testing.T) {
	b, cap, shared, _ := fixture(t)
	cmd := Command{Name: "worker", ProcessIdentity: "worker-process", ArgvIdentity: "worker-argv", Children: []Command{{Name: "child", ProcessIdentity: "child-process", ArgvIdentity: "child-argv", Children: []Command{{Name: "grandchild", ProcessIdentity: "grandchild-process", ArgvIdentity: "grandchild-argv", Paths: []string{filepath.Join(shared, "grandchild.out")}}}}}}
	if err := b.AuthorizeCommand(cap, cmd); err == nil {
		t.Fatal("grandchild outside-root write was authorized")
	}
}

func TestHardlinkDenied(t *testing.T) {
	b, cap, shared, _ := fixture(t)
	source := filepath.Join(shared, "outside-source")
	link := filepath.Join(cap.root, "hardlink")
	if err := os.WriteFile(source, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, link); err != nil {
		t.Fatal(err)
	}
	if err := b.AuthorizeWrite(cap, link); !errors.Is(err, ErrHardlink) {
		t.Fatalf("hardlink error = %v", err)
	}
}

func TestSentinelRevalidatedAndTupleBound(t *testing.T) {
	b, cap, _, _ := fixture(t)
	if err := os.WriteFile(cap.sentinel, []byte("dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.AuthorizeWrite(cap, filepath.Join(cap.root, "after-dirty")); !errors.Is(err, ErrInvalidSentinel) {
		t.Fatalf("dirty sentinel error = %v", err)
	}
	if err := os.WriteFile(cap.sentinel, []byte(sentinelContents), 0600); err != nil {
		t.Fatal(err)
	}
	for field, altered := range map[string]AuthTuple{
		"repository": {Repository: "other-repo", Task: cap.tuple.Task, Lane: cap.tuple.Lane, Session: cap.tuple.Session, PolicyDigest: cap.tuple.PolicyDigest},
		"task":       {Repository: cap.tuple.Repository, Task: "FAC-other", Lane: cap.tuple.Lane, Session: cap.tuple.Session, PolicyDigest: cap.tuple.PolicyDigest},
		"lane":       {Repository: cap.tuple.Repository, Task: cap.tuple.Task, Lane: "other-lane", Session: cap.tuple.Session, PolicyDigest: cap.tuple.PolicyDigest},
		"session":    {Repository: cap.tuple.Repository, Task: cap.tuple.Task, Lane: cap.tuple.Lane, Session: "other-session", PolicyDigest: cap.tuple.PolicyDigest},
		"policy":     {Repository: cap.tuple.Repository, Task: cap.tuple.Task, Lane: cap.tuple.Lane, Session: cap.tuple.Session, PolicyDigest: "policy-v2"},
	} {
		t.Run(field, func(t *testing.T) {
			forged := cap
			forged.tuple = altered
			if err := b.AuthorizeWrite(forged, filepath.Join(cap.root, "tuple-bound")); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("altered tuple error = %v", err)
			}
		})
	}
}

func TestSentinelReplacementDenied(t *testing.T) {
	b, cap, _, _ := fixture(t)
	replacement := filepath.Join(filepath.Dir(cap.sentinel), ".replacement")
	if err := os.WriteFile(replacement, []byte(sentinelContents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, cap.sentinel); err != nil {
		t.Fatal(err)
	}
	if err := b.AuthorizeWrite(cap, filepath.Join(cap.root, "replacement-target")); !errors.Is(err, ErrInvalidSentinel) {
		t.Fatalf("replacement sentinel error = %v", err)
	}
}

func TestCommandShapeBounded(t *testing.T) {
	b, cap, shared, _ := fixture(t)
	invalid := []Command{
		{ProcessIdentity: "p", ArgvIdentity: "a", Paths: []string{filepath.Join(shared, "x")}},
		{Name: "pathless", ProcessIdentity: "p", ArgvIdentity: "a"},
	}
	for _, command := range invalid {
		if err := b.AuthorizeCommand(cap, command); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("invalid command error = %v", err)
		}
	}
	deep := Command{Name: "0", ProcessIdentity: "p", ArgvIdentity: "a", Paths: []string{"ok"}}
	for i := 1; i < maxCommandDepth+1; i++ {
		deep = Command{Name: string(rune('0' + i)), ProcessIdentity: "p", ArgvIdentity: "a", Children: []Command{deep}}
	}
	if err := b.AuthorizeCommand(cap, deep); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("over-depth command error = %v", err)
	}
}

type unsupportedFileInfo struct{}

func (unsupportedFileInfo) Name() string       { return "unsupported" }
func (unsupportedFileInfo) Size() int64        { return 0 }
func (unsupportedFileInfo) Mode() os.FileMode  { return 0 }
func (unsupportedFileInfo) ModTime() time.Time { return time.Time{} }
func (unsupportedFileInfo) IsDir() bool        { return false }
func (unsupportedFileInfo) Sys() interface{}   { return nil }

func TestUnsupportedIdentityFailsClosed(t *testing.T) {
	if _, err := identityOfInfo(unsupportedFileInfo{}); !errors.Is(err, ErrUnsupportedIdentity) {
		t.Fatalf("unsupported identity error = %v", err)
	}
}

func TestZeroRootIdentityFailsClosed(t *testing.T) {
	if err := sameDevice(t.TempDir(), fileIdentity{}); !errors.Is(err, ErrUnsupportedIdentity) {
		t.Fatalf("zero root identity error = %v", err)
	}
}

func TestRootSentinelAndCapabilityAreSeparate(t *testing.T) {
	b, cap, shared, root := fixture(t)
	if err := b.AuthorizeWrite(Capability{}, filepath.Join(root, "ok")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("zero capability error = %v", err)
	}
	if _, _, err := New(root, shared, cap.tuple, testIssuer{}); !errors.Is(err, ErrInvalidSentinel) {
		t.Fatalf("root-as-sentinel error = %v", err)
	}
	dirtySentinel := filepath.Join(root, "dirty-sentinel")
	if err := os.WriteFile(dirtySentinel, []byte("not-authenticated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(root, dirtySentinel, cap.tuple, testIssuer{}); !errors.Is(err, ErrInvalidSentinel) {
		t.Fatalf("dirty root sentinel error = %v", err)
	}
	siblingSentinel := filepath.Join(shared, "sibling-sentinel")
	if err := os.WriteFile(siblingSentinel, []byte(sentinelContents), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(root, siblingSentinel, cap.tuple, testIssuer{}); !errors.Is(err, ErrInvalidSentinel) {
		t.Fatalf("sibling sentinel error = %v", err)
	}
	if err := b.AuthorizeWrite(Capability{root: cap.root, sentinel: cap.sentinel, tuple: cap.tuple}, filepath.Join(root, "ok")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("uncredentialed capability error = %v", err)
	}
}

func TestSymlinkedRootRejected(t *testing.T) {
	shared := t.TempDir()
	root := filepath.Join(shared, "root")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sentinel"), []byte(sentinelContents), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(shared, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(alias, filepath.Join(alias, "sentinel"), fixtureTuple(), testIssuer{}); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlinked root error = %v", err)
	}
}
