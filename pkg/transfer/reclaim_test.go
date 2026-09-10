package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/lock"
)

type reclaimFixture struct {
	t          *testing.T
	repoRoot   string
	bundleDir  string
	bundlePath string
	bundleSize int64
	manifest   string
	lockDir    string
}

func reclaimFixtureSetup(t *testing.T) reclaimFixture {
	t.Helper()
	repoRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	// .herd state is fleet-local and must never be tracked; a tracked manifest
	// or bundle would be deleted by a later reset --hard in tests.
	if err := os.WriteFile(filepath.Join(repoRoot, ".gitignore"), []byte(".herd/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "seed.txt"), []byte("seed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "seed")
	git("update-ref", "refs/remotes/origin/main", "main")

	bundleDir := filepath.Join(repoRoot, ".herd", "coordinator-resume", "completion-recovery-test")
	if err := os.MkdirAll(bundleDir, 0755); err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(bundleDir, "eligible-transfer.bundle")
	cmd := exec.Command("git", "-C", repoRoot, "bundle", "create", bundlePath, "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
	st, err := os.Stat(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	f := reclaimFixture{t: t, repoRoot: repoRoot, bundleDir: bundleDir, bundlePath: bundlePath, bundleSize: st.Size()}
	f.manifest = f.writeManifest("eligible-transfer.bundle")
	f.lockDir = filepath.Join(repoRoot, ".git", "herd-shared-checkout.lock.d")
	return f
}

func (f reclaimFixture) writeManifest(names ...string) string {
	f.t.Helper()
	body, err := json.Marshal(RetentionManifest{Version: 1, Authority: "forge-orchestrator-test", Bundles: names})
	if err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(f.bundleDir, "retention-manifest.json")
	if err := os.WriteFile(path, body, 0644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func absentReader() func(context.Context, string) (ReaderStatus, error) {
	return func(context.Context, string) (ReaderStatus, error) { return ReaderAbsent, nil }
}

func opts(f reclaimFixture, mutate func(*ReclaimOptions)) ReclaimOptions {
	o := ReclaimOptions{
		RepoRoot: f.repoRoot, Root: f.bundleDir, Manifest: f.manifest,
		LockDir: f.lockDir, LockWait: 5 * time.Second,
		LiveReader: absentReader(),
	}
	if mutate != nil {
		mutate(&o)
	}
	return o
}

func TestReclaimDryRunNeverDeletesOrClaimsFreedSpace(t *testing.T) {
	f := reclaimFixtureSetup(t)
	report, err := Reclaim(context.Background(), opts(f, nil))
	if err != nil {
		t.Fatal(err)
	}
	if report.DryRun != true || report.Candidates != 1 || report.Reclaimed != 0 || report.ReclaimedBytes != 0 {
		t.Fatalf("dry-run report wrong: %+v", report)
	}
	if report.WouldReclaimBytes != f.bundleSize {
		t.Fatalf("would-reclaim bytes = %d, want %d", report.WouldReclaimBytes, f.bundleSize)
	}
	if len(report.Dispositions) != 1 || report.Dispositions[0].Action != "eligible-candidate" {
		t.Fatalf("dispositions wrong: %+v", report.Dispositions)
	}
	if _, err := os.Lstat(f.bundlePath); err != nil {
		t.Fatalf("dry-run deleted the bundle: %v", err)
	}
}

func TestReclaimActRemovesEligibleBundleAndReadsBack(t *testing.T) {
	f := reclaimFixtureSetup(t)
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil {
		t.Fatal(err)
	}
	if report.Reclaimed != 1 || report.ReclaimedBytes != f.bundleSize || report.WouldReclaimBytes != 0 {
		t.Fatalf("act report wrong: %+v", report)
	}
	if _, err := os.Lstat(f.bundlePath); !os.IsNotExist(err) {
		t.Fatalf("bundle still present after act reclaim: %v", err)
	}
}

// Guard 1: native lock serialization.
func TestReclaimSerializesOnNativeLock(t *testing.T) {
	f := reclaimFixtureSetup(t)
	shared := lock.NewDirLock(f.lockDir)
	if err := shared.Acquire(context.Background(), 0, "foreign transfer holding the shared lock"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true; o.LockWait = 1200 * time.Millisecond }))
	if err == nil || !strings.Contains(err.Error(), "native lock") {
		t.Fatalf("reclaim must refuse while a participating transfer holds the native lock: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("reclaim did not wait for the native lock owner (waited %v)", elapsed)
	}
	if _, statErr := os.Lstat(f.bundlePath); statErr != nil {
		t.Fatal(statErr)
	}
	shared.Release()
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil || report.Reclaimed != 1 {
		t.Fatalf("reclaim after lock release must proceed: report=%+v err=%v", report, err)
	}
}

func TestReclaimNestedLockDoesNotReleaseForeignHolder(t *testing.T) {
	f := reclaimFixtureSetup(t)
	shared := lock.NewDirLock(f.lockDir)
	if err := shared.Acquire(context.Background(), 0, "outer holder"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(lock.EnvHeld, shared.Dir())
	if _, err := Reclaim(context.Background(), opts(f, nil)); err != nil {
		t.Fatal(err)
	}
	if held, _ := shared.Status(); !held {
		t.Fatal("re-entrant reclaim released the foreign lock holder's directory")
	}
	shared.Release()
}

// Guard 2: explicit retention-manifest authority.
func TestReclaimRetainsBundlesOutsideRetentionManifest(t *testing.T) {
	f := reclaimFixtureSetup(t)
	f.writeManifest("some-other.bundle")
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil {
		t.Fatal(err)
	}
	if report.Reclaimed != 0 || report.Candidates != 0 || report.Dispositions[0].Reason != "not-in-retention-manifest" {
		t.Fatalf("unmanifested bundle must be retained: %+v", report)
	}
	if _, err := os.Lstat(f.bundlePath); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimRequiresValidManifest(t *testing.T) {
	f := reclaimFixtureSetup(t)
	if _, err := Reclaim(context.Background(), ReclaimOptions{RepoRoot: f.repoRoot, Root: f.bundleDir, LockDir: f.lockDir}); err == nil || !strings.Contains(err.Error(), "retention manifest is required") {
		t.Fatalf("missing manifest must refuse: %v", err)
	}
	broken := filepath.Join(f.bundleDir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Manifest = broken })); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed manifest must refuse: %v", err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "elsewhere.json")
	if err := os.WriteFile(outside, []byte(`{"version":1,"authority":"x","bundles":["eligible-transfer.bundle"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Manifest = outside })); err == nil || !strings.Contains(err.Error(), "not inside the owned root") {
		t.Fatalf("manifest outside owned root must refuse: %v", err)
	}
}

// Guard 3: parent identity revalidation and conservative link handling.
func TestReclaimRetainsWhenParentDirectoryIdentityChanges(t *testing.T) {
	f := reclaimFixtureSetup(t)
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) {
		o.Act = true
		o.LiveReader = func(ctx context.Context, path string) (ReaderStatus, error) {
			moved := f.bundleDir + ".moved"
			if err := os.Rename(f.bundleDir, moved); err != nil {
				return ReaderUnknown, err
			}
			if err := os.Mkdir(f.bundleDir, 0755); err != nil {
				return ReaderUnknown, err
			}
			return ReaderAbsent, nil
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if report.Reclaimed != 0 || !strings.HasPrefix(report.Dispositions[0].Reason, "parent-changed-during-reclaim") {
		t.Fatalf("parent replacement must retain: %+v", report)
	}
	if _, err := os.Lstat(filepath.Join(f.bundleDir+".moved", "eligible-transfer.bundle")); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimRetainsHardLinkedBundle(t *testing.T) {
	f := reclaimFixtureSetup(t)
	if err := os.Link(f.bundlePath, filepath.Join(f.bundleDir, "hardlinked.bundle")); err != nil {
		t.Fatal(err)
	}
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range report.Dispositions {
		if d.Action == "reclaimed" {
			t.Fatalf("hard-linked inode must be retained conservatively: %+v", report)
		}
	}
	if _, err := os.Lstat(f.bundlePath); err != nil {
		t.Fatal(err)
	}
}

// Guard 4: bounded process lifetime and output.
func TestReclaimBoundedListHeadsOutputOverflowRetains(t *testing.T) {
	f := reclaimFixtureSetup(t)
	fakeBin := t.TempDir()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = bundle ] && [ \"$2\" = list-heads ]; then\n  awk 'BEGIN{for(i=0;i<400000;i++) printf \"%%040x ref-%%08d\\n\", i, i}'\n  exit 0\nfi\nexec %s \"$@\"\n", realGit)
	if err := os.WriteFile(filepath.Join(fakeBin, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil {
		t.Fatal(err)
	}
	if report.Reclaimed != 0 || !strings.Contains(report.Dispositions[0].Reason, "bundle-tips-unknown") ||
		!strings.Contains(report.Dispositions[0].Reason, "exceeded") {
		t.Fatalf("oversized list-heads output must be retained by the output bound: %+v", report)
	}
}

func TestDefaultLsofReaderTimesOutHungProcess(t *testing.T) {
	f := reclaimFixtureSetup(t)
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "lsof"), []byte("#!/bin/sh\n/bin/sleep 5\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin)
	orig := defaultLsofTimeout
	defaultLsofTimeout = 200 * time.Millisecond
	started := time.Now()
	status, err := DefaultLsofReader(context.Background(), f.bundlePath)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("hung lsof must be bounded, took %v", elapsed)
	}
	if status != ReaderUnknown || err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("bounded hang must report an explicit timeout unknown: %v %v", status, err)
	}
	// RED control: without the deadline the same reader blocks for 5s.
	defaultLsofTimeout = orig
	started = time.Now()
	_, _ = DefaultLsofReader(context.Background(), f.bundlePath)
	if elapsed := time.Since(started); elapsed < 3*time.Second {
		t.Fatalf("RED control: unbounded reader returned early (%v); the deadline is unproven", elapsed)
	}
}

// Guard 4/5: bounded directory iteration stays under the file budget.
func TestReclaimStopsEnumerationAtFileBudget(t *testing.T) {
	f := reclaimFixtureSetup(t)
	for i := 0; i < 3; i++ {
		src := filepath.Join(f.bundleDir, fmt.Sprintf("extra-%d.bundle", i))
		body, err := os.ReadFile(f.bundlePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(src, body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	f.writeManifest("eligible-transfer.bundle", "extra-0.bundle", "extra-1.bundle", "extra-2.bundle")
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.MaxFiles = 2 }))
	if err != nil || report.Scanned != 2 || !report.Partial || report.Reason != "file-budget" {
		t.Fatalf("file budget not honored: %+v err=%v", report, err)
	}
	if len(report.Dispositions) != 2 {
		t.Fatalf("only budgeted bundles may be dispositioned: %+v", report.Dispositions)
	}
}

// Guard 5: strict list-heads parsing.
func TestParseListHeadsOutputRejectsMalformedLines(t *testing.T) {
	valid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main\nbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/herd/rescue/x\n"
	tips, err := parseListHeadsOutput(valid)
	if err != nil || len(tips) != 2 {
		t.Fatalf("valid output must parse: %v %v", tips, err)
	}
	for name, bad := range map[string]string{
		"garbage":      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main\nGARBAGE-LINE\n",
		"short-sha":    "abc123 refs/heads/main\n",
		"non-hex-sha":  "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz refs/heads/main\n",
		"missing-ref":  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		"three-fields": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main extra\n",
		"empty":        "\n\n",
	} {
		if _, err := parseListHeadsOutput(bad); err == nil {
			t.Fatalf("%s: malformed list-heads output must be rejected, not skipped", name)
		}
	}
}

func TestReclaimRetainsBundleWithUnreachableTips(t *testing.T) {
	f := reclaimFixtureSetup(t)
	if err := os.WriteFile(filepath.Join(f.repoRoot, "child.txt"), []byte("orphan\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = f.repoRoot
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("add", ".")
	git("commit", "-q", "-m", "child")
	orphan := filepath.Join(f.bundleDir, "orphan-tip.bundle")
	if out, err := exec.Command("git", "-C", f.repoRoot, "bundle", "create", orphan, "main").CombinedOutput(); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
	git("reset", "-q", "--hard", "HEAD~1")
	git("update-ref", "refs/remotes/origin/main", "main")
	f.writeManifest("eligible-transfer.bundle", "orphan-tip.bundle")
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil {
		t.Fatal(err)
	}
	retained := false
	for _, d := range report.Dispositions {
		if d.Name == "orphan-tip.bundle" {
			if d.Action != "retained" || !strings.Contains(d.Reason, "tips-not-retained-by-canonical-refs") {
				t.Fatalf("orphan-tip bundle must be retained with reason, got %+v", d)
			}
			retained = true
		}
	}
	if !retained {
		t.Fatalf("bundle with tip not retained by canonical refs was handled wrong: %+v", report)
	}
	if _, err := os.Lstat(orphan); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimRetainsBundleWithObjectsMissingFromCanonical(t *testing.T) {
	f := reclaimFixtureSetup(t)
	other := t.TempDir()
	cmd := exec.Command("sh", "-c", "git init -q -b main other && cd other && git config user.email o@o && git config user.name o && echo unique > u.txt && git add . && git commit -q -m unique && git bundle create ../unique.bundle main")
	cmd.Dir = other
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(other, "unique.bundle"))
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(f.bundleDir, "foreign-history.bundle")
	if err := os.WriteFile(foreign, body, 0644); err != nil {
		t.Fatal(err)
	}
	f.writeManifest("eligible-transfer.bundle", "foreign-history.bundle")
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range report.Dispositions {
		if d.Name == "foreign-history.bundle" {
			if d.Action != "retained" || !strings.Contains(d.Reason, "tip-missing-from-canonical") {
				t.Fatalf("bundle with unique objects must be retained with reason, got %+v", d)
			}
		}
	}
	if _, err := os.Lstat(foreign); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimRetainsWhenReaderPresentOrUnknown(t *testing.T) {
	f := reclaimFixtureSetup(t)
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) {
		o.Act = true
		o.LiveReader = func(context.Context, string) (ReaderStatus, error) { return ReaderPresent, nil }
	}))
	if err != nil || report.Reclaimed != 0 || report.Dispositions[0].Reason != "reader-present" {
		t.Fatalf("reader-present must retain: report=%+v err=%v", report, err)
	}
	if _, err := os.Lstat(f.bundlePath); err != nil {
		t.Fatal(err)
	}
	report, err = Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) {
		o.Act = true
		o.LiveReader = func(context.Context, string) (ReaderStatus, error) {
			return ReaderUnknown, errors.New("lsof unavailable")
		}
	}))
	if err != nil || report.Reclaimed != 0 || !strings.HasPrefix(report.Dispositions[0].Reason, "reader-proof-unknown") {
		t.Fatalf("reader-unknown must retain fail-closed: report=%+v err=%v", report, err)
	}
	if _, err := os.Lstat(f.bundlePath); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimProtectListRetainsNamedBundle(t *testing.T) {
	f := reclaimFixtureSetup(t)
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) {
		o.Act = true
		o.Protect = []string{"eligible-transfer.bundle"}
	}))
	if err != nil || report.Reclaimed != 0 || report.Dispositions[0].Reason != "explicitly-protected" {
		t.Fatalf("protected bundle must be retained: %+v err=%v", report, err)
	}
	if _, err := os.Lstat(f.bundlePath); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimRevalidatesIdentityImmediatelyBeforeUnlink(t *testing.T) {
	f := reclaimFixtureSetup(t)
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) {
		o.Act = true
		o.LiveReader = func(ctx context.Context, path string) (ReaderStatus, error) {
			if fh, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0); openErr == nil {
				_, _ = fh.WriteString("drift")
				_ = fh.Close()
			}
			return ReaderAbsent, nil
		}
	}))
	if err != nil || report.Reclaimed != 0 || !strings.HasPrefix(report.Dispositions[0].Reason, "changed-during-reclaim") {
		t.Fatalf("changed bundle must be retained at revalidation: %+v err=%v", report, err)
	}
	if _, err := os.Lstat(f.bundlePath); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimScopeRefusals(t *testing.T) {
	f := reclaimFixtureSetup(t)
	outside := t.TempDir()
	if _, err := Reclaim(context.Background(), ReclaimOptions{RepoRoot: f.repoRoot, Root: outside, Manifest: f.manifest}); err == nil || !strings.Contains(err.Error(), "not inside the repository") {
		t.Fatalf("root outside repo must refuse: %v", err)
	}
	naked := filepath.Join(f.repoRoot, "not-owned", "bundles")
	if err := os.MkdirAll(naked, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Reclaim(context.Background(), ReclaimOptions{RepoRoot: f.repoRoot, Root: naked, Manifest: f.manifest}); err == nil || !strings.Contains(err.Error(), "not inside owned .herd state") {
		t.Fatalf("root without .herd ownership must refuse: %v", err)
	}
	if _, err := Reclaim(context.Background(), ReclaimOptions{RepoRoot: "", Root: f.bundleDir, Manifest: f.manifest}); err == nil {
		t.Fatal("missing repo identity must refuse")
	}
}

func TestReclaimBudgetsBoundOnePass(t *testing.T) {
	f := reclaimFixtureSetup(t)
	second := filepath.Join(f.bundleDir, "second.bundle")
	if out, err := exec.Command("git", "-C", f.repoRoot, "bundle", "create", second, "main").CombinedOutput(); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
	f.writeManifest("eligible-transfer.bundle", "second.bundle")
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.MaxFiles = 1 }))
	if err != nil || report.Scanned != 1 || !report.Partial || report.Reason != "file-budget" {
		t.Fatalf("file budget not honored: %+v err=%v", report, err)
	}
	report, err = Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.MaxBytes = 1 }))
	if err != nil || !report.Partial || report.Reason != "byte-budget" || report.Candidates != 0 {
		t.Fatalf("byte budget not honored: %+v err=%v", report, err)
	}
}

func TestReclaimMinAgeRetainsActiveTransfers(t *testing.T) {
	f := reclaimFixtureSetup(t)
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.MinAge = time.Hour }))
	if err != nil || report.Candidates != 0 || report.Dispositions[0].Reason != "recently-modified-active-transfer-window" {
		t.Fatalf("min-age must retain fresh transfers: %+v err=%v", report, err)
	}
}

func TestDefaultLsofReaderParsesHermeticOutput(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   ReaderStatus
	}{
		{"present", "#!/bin/sh\necho 'herd 4242 bundle'\n", ReaderPresent},
		{"absent", "#!/bin/sh\nexit 1\n", ReaderAbsent},
		{"unknown-error", "#!/bin/sh\nexit 2\n", ReaderUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(binDir, "lsof"), []byte(tc.script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir)
			status, err := DefaultLsofReader(context.Background(), filepath.Join(t.TempDir(), "x.bundle"))
			if status != tc.want {
				t.Fatalf("status = %q want %q (err=%v)", status, tc.want, err)
			}
			if tc.want == ReaderPresent && err != nil {
				t.Fatalf("present must not error: %v", err)
			}
		})
	}
	lsofMissing := t.TempDir()
	t.Setenv("PATH", lsofMissing)
	if status, err := DefaultLsofReader(context.Background(), "x"); status != ReaderUnknown || err == nil {
		t.Fatalf("missing lsof must be unknown, never absent: %v %v", status, err)
	}
}
