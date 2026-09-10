package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-769: the packet must describe the actual exclusive leased worktree, its
// permitted evidence delivery, and the exact review base/head, so native
// W4/custom-pool reviews do not stop on contradictory instructions or silently
// review the last delta.
//
// A1: a custom pool-root packet names its actual leased slot and never treats
// the literal ".herd/pool/" substring as isolation authority.

// fac769Fixture builds a tiny repo with an origin/main and a candidate commit
// on a feature branch, so merge-base resolution is real and hermetic.
func fac769Fixture(t *testing.T) (root, base, candidate string) {
	t.Helper()
	root = t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-qm", "base")
	base = gitIn(t, root, "rev-parse", "HEAD")
	gitIn(t, root, "branch", "-M", "main")
	gitIn(t, root, "branch", "origin/main", "main")
	gitIn(t, root, "checkout", "-q", "-b", "feat/candidate")
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-qm", "candidate")
	candidate = gitIn(t, root, "rev-parse", "HEAD")
	return root, base, candidate
}

// A1: a custom pool-root packet names its actual leased slot path, not a
// literal default ".herd/pool/pool-NN". The isolation authority is the actual
// leased slot, never a substring match on ".herd/pool/".
func TestFac769PacketNamesActualLeasedSlotForCustomPoolRoot(t *testing.T) {
	body := reviewPacketBody("FAC-769", strings.Repeat("a", 40),
		"base", "surface", "/custom/pool-root/pool-07",
		"/repo/.herd/review/inbox/v.md", "review-supervisor", "openai", "w2", "FAC-769")

	if !strings.Contains(body, "/custom/pool-root/pool-07") {
		t.Fatal("packet must name the actual custom leased slot path, not a literal default")
	}
	if strings.Contains(body, ".herd/pool/pool-NN") {
		t.Fatal("packet must not hardcode the literal default pool slot placeholder")
	}
	// The isolation prose must not assert that ".herd/pool/" is the authority;
	// it must reference the actual leased slot.
	if strings.Contains(body, "not under .herd/pool/") {
		t.Fatal("packet must not treat the literal .herd/pool/ substring as isolation authority")
	}
	if !strings.Contains(body, "not under the pool root") {
		t.Fatal("packet must describe isolation as the pool root, not a literal substring")
	}
}

// A2: the packet explicitly permits ONLY its canonical inbox artifact write plus
// native evidence transport outside the source surface, while source mutation
// stays exclusive.
func TestFac769PacketPermitsOnlyCanonicalWriteAndTransport(t *testing.T) {
	body := reviewPacketBody("FAC-769", strings.Repeat("a", 40),
		"base", "surface", ".herd/pool/pool-01",
		"/repo/.herd/review/inbox/v.md", "review-supervisor", "openai", "w2", "FAC-769")

	for _, want := range []string{
		"writing your verdict artifact to the canonical inbox path below",
		"transporting that verdict home",
		"verdict-push",
		"herd mail send",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("packet must explicitly sanction %q as an out-of-surface operation", want)
		}
	}
	if !strings.Contains(body, "Everything else — every git command, build, and test — stays inside your\nexclusive leased pool slot") {
		t.Fatal("packet must keep all source mutation exclusive to the leased slot")
	}
}

// A3: the packet carries the exact reviewed-base from the validated launch pin,
// and the printed range matches it. A wrong or unresolved base refuses
// generation before launch.
func TestFac769PacketCarriesExactReviewedBase(t *testing.T) {
	base := strings.Repeat("b", 40)
	body := reviewPacketBody("FAC-769", strings.Repeat("a", 40),
		base, "surface", ".herd/pool/pool-01",
		"/repo/.herd/review/inbox/v.md", "review-supervisor", "openai", "w2", "FAC-769")

	if !strings.Contains(body, "reviewed-base: "+base) {
		t.Fatal("packet must prefill the exact reviewed-base from the launch pin")
	}
	if !strings.Contains(body, "Review the whole\nrange from that base to the candidate head") {
		t.Fatal("packet must instruct reviewing the whole base-to-head range")
	}
}

// A3: resolveReviewBase resolves the exact merge-base against origin/main and
// fails closed on an unresolved or invalid base.
func TestFac769ResolveReviewBaseResolvesExactMergeBase(t *testing.T) {
	root, base, candidate := fac769Fixture(t)
	got, err := resolveReviewBase(root, "", "FAC-769", candidate, base)
	if err != nil {
		t.Fatalf("resolveReviewBase failed: %v", err)
	}
	if got != base {
		t.Fatalf("resolveReviewBase = %q, want exact merge-base %q", got, base)
	}
}

// A3: an unresolved base (no origin/main) must refuse generation, never invent
// a base from the latest parent.
func TestFac769ResolveReviewBaseFailsClosedWithoutAuthenticatedOrExplicitBase(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-qm", "base")
	candidate := gitIn(t, root, "rev-parse", "HEAD")

	if _, err := resolveReviewBase(root, root, "FAC-769", candidate, ""); err == nil {
		t.Fatal("an unresolved base (no authenticated task context or explicit base) must refuse generation")
	}
}

func TestFac769ExplicitBaseSurvivesOriginMainAdvance(t *testing.T) {
	root, base, candidate := fac769Fixture(t)
	gitIn(t, root, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("advanced"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-qm", "advance main")
	gitIn(t, root, "update-ref", "refs/heads/origin/main", candidate)
	got, err := resolveReviewBase(root, "", "FAC-769", candidate, base)
	if err != nil {
		t.Fatalf("resolveReviewBase failed after origin/main advance: %v", err)
	}
	if got != base {
		t.Fatalf("resolveReviewBase = %q after origin/main advance, want pinned %q", got, base)
	}
}

// A3 wiring: runPoolReview must resolve the base BEFORE rendering the packet,
// and must pass the resolved base and the actual leased slot path into the
// packet body. Source-scan in the established style.
func TestFac769BaseResolutionPrecedesPacketAndThreadsSlot(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := funcBody(string(src), "func runPoolReview(")
	if !ok {
		t.Fatal("cannot locate runPoolReview")
	}
	baseIdx := strings.Index(body, "resolveReviewBase(root, candidateDir, ref, sha, strings.TrimSpace(*opts.Base))")
	packetIdx := strings.Index(body, "reviewPacketBody(ref, sha, base, surface, lease.Path")
	if baseIdx < 0 {
		t.Fatal("runPoolReview never resolves the review base; the packet cannot carry the exact pin")
	}
	if packetIdx < 0 {
		t.Fatal("runPoolReview does not pass the resolved base and actual leased slot path into the packet body")
	}
	if baseIdx > packetIdx {
		t.Fatal("base resolution runs AFTER packet rendering; a wrong/unresolved base would not refuse generation before launch")
	}
}

// A4: the default-pool path still names a real leased slot and the
// supervisor-local (mail) delivery still works.
func TestFac769DefaultPoolAndSupervisorMailStillWork(t *testing.T) {
	body := reviewPacketBody("FAC-769", strings.Repeat("a", 40),
		"base", "surface", ".herd/pool/pool-01",
		"/repo/.herd/review/inbox/v.md", "forge-review-harvest-su-467b70d7", "openai", "w2", "FAC-769")

	if !strings.Contains(body, ".herd/pool/pool-01") {
		t.Fatal("default pool-root packet must still name its actual leased slot")
	}
	if !strings.Contains(body, "herd mail send --from") {
		t.Fatal("a reachable supervisor must still get a direct mail report")
	}
	if strings.Contains(body, "MAIL WILL NOT WORK") {
		t.Fatal("the unreachable-host warning must not appear when mail works")
	}
}
