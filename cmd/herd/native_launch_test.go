package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/herdr"
)

// isolateNativeLaunchFixture retargets process Git (DIR/config/remotes) onto a
// temporary repository and hides the live herdr binary behind the protocol
// fake. The two named native-launch tests must call this before any census or
// herdr RPC so a managed verifier's inherited GIT_DIR / PATH cannot mutate the
// shared root checkout.
func isolateNativeLaunchFixture(t *testing.T) (bin string, calls func() []string) {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "file:///dev/null/origin.git"},
	} {
		if out, err := testgit.Command(repo, args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture git %v: %v (%s)", args, err, out)
		}
	}
	gitDir := filepath.Join(repo, ".git")
	t.Setenv("GIT_DIR", gitDir)
	t.Setenv("GIT_WORK_TREE", repo)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "")
	bin, calls = installProtocolFakeHerdr(t)
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin, calls
}

func isolatedGitState(t *testing.T, repo string) string {
	t.Helper()
	configOut, err := testgit.Command(repo, "config", "--local", "--null", "--list").Output()
	if err != nil {
		t.Fatalf("git config --list: %v", err)
	}
	remoteOut, err := testgit.Command(repo, "remote", "-v").Output()
	if err != nil {
		t.Fatalf("git remote -v: %v", err)
	}
	return string(configOut) + "\x00---remotes---\x00" + string(remoteOut)
}

func unisolatedGitTouch() error {
	if _, err := exec.Command("git", "config", "--local", "herd.fac731.leak", "1").CombinedOutput(); err != nil {
		return err
	}
	if _, err := exec.Command("git", "remote", "add", "fac731-leak", "file:///dev/null/fac731-leak.git").CombinedOutput(); err != nil {
		return err
	}
	return nil
}

func TestWaitExactPaneBeforeStart_UnknownPaneIsLaunchFailed(t *testing.T) {
	before := liveWorkspaceCensus(t)
	_, _ = installProtocolFakeHerdr(t)
	t.Setenv("HERD_FAKE_PANE_MODE", "unknown")
	cwd := t.TempDir()
	tab, err := herdr.TabCreateForTask("wFAKE", "review-unknown", cwd, true)
	if err != nil {
		t.Fatalf("tab create: %v", err)
	}
	obs, waitErr := waitExactPaneBeforeStart(tab, 8*time.Millisecond)
	if !herdr.IsLaunchFailed(waitErr) {
		t.Fatalf("err=%v, want LAUNCH_FAILED", waitErr)
	}
	if obs.State != herdr.LaunchFailed || !strings.Contains(obs.Reason, "unknown pane") {
		t.Fatalf("obs=%+v, want unknown-pane LAUNCH_FAILED", obs)
	}
	assertLiveFleetUntouched(t, before)
}

func TestWaitExactPaneBeforeStart_AuthScreenIsLaunchFailed(t *testing.T) {
	_, _ = isolateNativeLaunchFixture(t)
	t.Setenv("HERD_FAKE_PANE_TITLE", "Sign in to continue")
	t.Setenv("HERD_FAKE_PANE_BODY", "please log in")
	cwd := t.TempDir()
	tab, err := herdr.TabCreateForTask("wFAKE", "review-auth", cwd, true)
	if err != nil {
		t.Fatalf("tab create: %v", err)
	}
	obs, waitErr := waitExactPaneBeforeStart(tab, time.Second)
	if !herdr.IsLaunchFailed(waitErr) {
		t.Fatalf("err=%v, want LAUNCH_FAILED", waitErr)
	}
	if obs.State != herdr.LaunchFailed || !strings.Contains(obs.Reason, "authentication") {
		t.Fatalf("obs=%+v, want auth-screen LAUNCH_FAILED", obs)
	}
}

func TestCompensateExactLaunchTab_DoesNotLeaveOrphan(t *testing.T) {
	_, calls := isolateNativeLaunchFixture(t)
	cwd := t.TempDir()
	tab, err := herdr.TabCreateForTask("wFAKE", "review-close", cwd, true)
	if err != nil {
		t.Fatalf("tab create: %v", err)
	}
	tab.Generation = "7"
	if err := compensateExactLaunchTab("wFAKE", tab); err != nil {
		t.Fatalf("compensateExactLaunchTab: %v", err)
	}
	if err := herdr.TabClose(tab.ID); err == nil {
		t.Fatal("public TabClose must stay refused")
	}
	sawCAS := false
	for _, c := range calls() {
		if strings.Contains(c, "tab compare-close") {
			sawCAS = true
		}
	}
	if !sawCAS {
		t.Fatalf("expected generation-safe compare-close, fake saw: %v", calls())
	}
}

// Removing isolateNativeLaunchFixture from the named tests lets inherited
// GIT_DIR receive config/remote writes (RED). Restoring it keeps the decoy
// "live root" and the real shared root unchanged (GREEN).
func TestNativeLaunchFixtureIsolation_WatchedRootGitGuard(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	rootBefore := isolatedGitState(t, root)

	t.Run("RED_without_isolation", func(t *testing.T) {
		decoy := t.TempDir()
		if out, err := testgit.Command(decoy, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("decoy init: %v (%s)", err, out)
		}
		before := isolatedGitState(t, decoy)
		t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
		t.Setenv("GIT_WORK_TREE", decoy)
		if err := unisolatedGitTouch(); err != nil {
			t.Fatalf("unisolated git touch: %v", err)
		}
		if isolatedGitState(t, decoy) == before {
			t.Fatal("removing fixture isolation must mutate inherited GIT_DIR config/remotes")
		}
	})

	t.Run("GREEN_with_isolation", func(t *testing.T) {
		decoy := t.TempDir()
		if out, err := testgit.Command(decoy, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("decoy init: %v (%s)", err, out)
		}
		before := isolatedGitState(t, decoy)
		t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
		t.Setenv("GIT_WORK_TREE", decoy)
		_, _ = isolateNativeLaunchFixture(t)
		if err := unisolatedGitTouch(); err != nil {
			t.Fatalf("isolated git touch: %v", err)
		}
		if isolatedGitState(t, decoy) != before {
			t.Fatal("fixture isolation must leave inherited GIT_DIR config/remotes unchanged")
		}
		if isolatedGitState(t, root) != rootBefore {
			t.Fatal("fixture isolation must leave the shared root git config/remotes unchanged")
		}
	})
}
