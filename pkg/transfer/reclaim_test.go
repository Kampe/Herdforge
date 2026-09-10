package transfer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reclaimFixture struct {
	t          *testing.T
	repoRoot   string
	bundleDir  string
	bundlePath string
	bundleSize int64
}

func reclaimFixtureSetup(t *testing.T) reclaimFixture {
	t.Helper()
	repoRoot := t.TempDir()
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
	return reclaimFixture{t: t, repoRoot: repoRoot, bundleDir: bundleDir, bundlePath: bundlePath, bundleSize: st.Size()}
}

func absentReader() func(context.Context, string) (ReaderStatus, error) {
	return func(context.Context, string) (ReaderStatus, error) { return ReaderAbsent, nil }
}

func opts(f reclaimFixture, mutate func(*ReclaimOptions)) ReclaimOptions {
	o := ReclaimOptions{RepoRoot: f.repoRoot, Root: f.bundleDir, LiveReader: absentReader()}
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

func TestReclaimNeverCandidatesNonBundleEvidence(t *testing.T) {
	f := reclaimFixtureSetup(t)
	receipts := map[string]string{
		"fac736-acceptance-authority.md":      "matrix",
		"fac736-evidence-delivery.json":       "{}",
		"fac736-native-close.log":             "log",
		"coordinator-recovery-record-0900.md": "recovery record",
	}
	for name, body := range receipts {
		if err := os.WriteFile(filepath.Join(f.bundleDir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	report, err := Reclaim(context.Background(), opts(f, nil))
	if err != nil {
		t.Fatal(err)
	}
	if report.NonBundleEntries != len(receipts) || report.Candidates != 1 || len(report.Dispositions) != 1 {
		t.Fatalf("non-bundle evidence was treated as candidate: %+v", report)
	}
	for name := range receipts {
		if _, err := os.Lstat(filepath.Join(f.bundleDir, name)); err != nil {
			t.Fatalf("receipt %s touched: %v", name, err)
		}
	}
}

func TestReclaimRetainsSymlinkedBundle(t *testing.T) {
	f := reclaimFixtureSetup(t)
	receipt := filepath.Join(f.bundleDir, "matrix-receipt.json")
	if err := os.WriteFile(receipt, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.bundleDir, "linked.bundle")
	if err := os.Symlink(receipt, link); err != nil {
		t.Fatal(err)
	}
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range report.Dispositions {
		if d.Name == "linked.bundle" {
			found = d.Action == "retained" && strings.Contains(d.Reason, "not-regular-file")
		}
	}
	if !found {
		t.Fatalf("symlink disposition missing: %+v", report.Dispositions)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink itself must be retained, not unlinked: %v", err)
	}
	if body, err := os.ReadFile(receipt); err != nil || string(body) != "{}" {
		t.Fatalf("link target disturbed: %v", err)
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
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil {
		t.Fatal(err)
	}
	retained, reclaimed := false, 0
	for _, d := range report.Dispositions {
		if d.Name == "orphan-tip.bundle" {
			if d.Action != "retained" || !strings.Contains(d.Reason, "tips-not-retained-by-canonical-refs") {
				t.Fatalf("orphan-tip bundle must be retained with reason, got %+v", d)
			}
			retained = true
		}
		if d.Action == "reclaimed" {
			reclaimed++
		}
	}
	if !retained || reclaimed != 0 {
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
	if _, err := Reclaim(context.Background(), ReclaimOptions{RepoRoot: f.repoRoot, Root: outside}); err == nil || !strings.Contains(err.Error(), "not inside the repository") {
		t.Fatalf("root outside repo must refuse: %v", err)
	}
	naked := filepath.Join(f.repoRoot, "not-owned", "bundles")
	if err := os.MkdirAll(naked, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Reclaim(context.Background(), ReclaimOptions{RepoRoot: f.repoRoot, Root: naked}); err == nil || !strings.Contains(err.Error(), "not inside owned .herd state") {
		t.Fatalf("root without .herd ownership must refuse: %v", err)
	}
	if _, err := Reclaim(context.Background(), ReclaimOptions{RepoRoot: "", Root: f.bundleDir}); err == nil {
		t.Fatal("missing repo identity must refuse")
	}
}

func TestReclaimBudgetsBoundOnePass(t *testing.T) {
	f := reclaimFixtureSetup(t)
	second := filepath.Join(f.bundleDir, "second.bundle")
	if out, err := exec.Command("git", "-C", f.repoRoot, "bundle", "create", second, "main").CombinedOutput(); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
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
		{"unknown-empty-success", "#!/bin/sh\nexit 0\n", ReaderUnknown},
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
