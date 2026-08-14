package herdr

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoginOrAuthScreen_DetectsCodexBrowserLogin(t *testing.T) {
	cases := []struct {
		title, body string
		want        bool
	}{
		{"codex · browser-login", "", true},
		{"Codex", "Please log in to continue\nVisit https://chatgpt.com/...", true},
		{"lp-codex-29000", "Sign in with your OpenAI account", true},
		{"working", "implement FAC-133 tool write", false},
		{"", "LIVE_TOOL_abc session=ses_x", false},
	}
	for _, c := range cases {
		if got := LoginOrAuthScreen(c.title, c.body); got != c.want {
			t.Errorf("LoginOrAuthScreen(%q,%q)=%v want %v", c.title, c.body, got, c.want)
		}
	}
}

func TestRealModelSessionID_RejectsFallbacks(t *testing.T) {
	bad := []string{
		"", "pending-x", "ses_probe_1", "ses_real_1", "ses_spawn_wK_t9",
		"test-session-t", "herdr-term:term_abc", "herdr-pane:wF:p1",
	}
	for _, s := range bad {
		if RealModelSessionID(s) {
			t.Errorf("%q must not count as real model session", s)
		}
	}
	if !RealModelSessionID("019fc450-7ce2-7602-a62c-329f31271c7a") {
		t.Fatal("uuid-like codex session should be accepted")
	}
	if !RealModelSessionID("ses_03cada7f7ffe0a0iK2e9LvZ1eT") {
		t.Fatal("opencode-style session should be accepted")
	}
}

func TestRejectNonModelSession_LoginBlocks(t *testing.T) {
	// Exercise rejectNonModelSession (not only LoginOrAuthScreen).
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "agent list") {
			return `{"result":{"agents":[{"name":"lp-codex","agent":"codex","agent_status":"idle","tab_id":"wF:t51","terminal_title":"browser-login","agent_session":{"value":"ses_x"}}],"type":"agent_list"}}`, nil
		}
		if strings.HasPrefix(joined, "agent read") {
			return "Please log in to continue at https://chatgpt.com", nil
		}
		return "", fmt.Errorf("unexpected herdr argv: %v", args)
	}
	blocker, err := rejectNonModelSession("lp-codex", "wF:t51", "ses_x")
	if err == nil {
		t.Fatal("expected login/auth failure from rejectNonModelSession")
	}
	if !strings.Contains(blocker, "login/auth") && !strings.Contains(blocker, "FAC-133 BLOCKED") {
		t.Fatalf("blocker=%q", blocker)
	}
}

func TestRejectNonModelSession_NoRealSession(t *testing.T) {
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "agent list") {
			return `{"result":{"agents":[{"name":"a","agent":"grok","agent_status":"idle","tab_id":"t1","agent_session":{"value":"herdr-term:x"}}],"type":"agent_list"}}`, nil
		}
		if strings.HasPrefix(joined, "agent read") {
			return "ready", nil
		}
		return "", fmt.Errorf("unexpected: %v", args)
	}
	blocker, err := rejectNonModelSession("a", "t1", "herdr-term:x")
	if err == nil || !strings.Contains(blocker, "no real model") {
		t.Fatalf("blocker=%q err=%v", blocker, err)
	}
}

func TestRejectNonModelSession_AGYLoginBlocks(t *testing.T) {
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "agent list") {
			return `{"result":{"agents":[{"name":"lp-agy","agent":"agy","agent_status":"idle","tab_id":"wF:t52","terminal_title":"Sign in with Google","agent_session":{"value":"ses_x"}}],"type":"agent_list"}}`, nil
		}
		if strings.HasPrefix(joined, "agent read") {
			return "Please sign in to continue with Google Antigravity", nil
		}
		return "", fmt.Errorf("unexpected herdr argv: %v", args)
	}
	blocker, err := rejectNonModelSession("lp-agy", "wF:t52", "ses_x")
	if err == nil {
		t.Fatal("expected login/auth failure for AGY from rejectNonModelSession")
	}
	if !strings.Contains(blocker, "login/auth") && !strings.Contains(blocker, "FAC-133 BLOCKED") {
		t.Fatalf("blocker=%q", blocker)
	}
}

func TestRejectNonModelSession_AGYNoRealSession(t *testing.T) {
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	runHerdr = func(args ...string) (string, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "agent list") {
			return `{"result":{"agents":[{"name":"lp-agy","agent":"agy","agent_status":"idle","tab_id":"t1","agent_session":{"value":"herdr-term:pane1"}}],"type":"agent_list"}}`, nil
		}
		if strings.HasPrefix(joined, "agent read") {
			return "ready", nil
		}
		return "", fmt.Errorf("unexpected: %v", args)
	}
	blocker, err := rejectNonModelSession("lp-agy", "t1", "herdr-term:pane1")
	if err == nil || !strings.Contains(blocker, "no real model") {
		t.Fatalf("blocker=%q err=%v", blocker, err)
	}
}
