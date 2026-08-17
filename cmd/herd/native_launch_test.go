package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

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
	before := liveWorkspaceCensus(t)
	_, _ = installProtocolFakeHerdr(t)
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
	assertLiveFleetUntouched(t, before)
}

func TestCompensateExactLaunchTab_DoesNotLeaveOrphan(t *testing.T) {
	before := liveWorkspaceCensus(t)
	_, calls := installProtocolFakeHerdr(t)
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
	assertLiveFleetUntouched(t, before)
}
