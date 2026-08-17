package herdr

import "testing"

func TestDetectContextWarningOnlyMatchesExplicitPressure(t *testing.T) {
	if got := DetectContextWarning("tokens used: 12000\ncontext window exceeded: request too large"); got != "context window exceeded: request too large" {
		t.Fatalf("warning=%q", got)
	}
	if got := DetectContextWarning("normal idle pane with 12000 tokens"); got != "" {
		t.Fatalf("ordinary token count produced warning %q", got)
	}
}

func TestExplainAgentDecodesStructuredDetection(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		jsonFlag := false
		for _, arg := range args {
			if arg == "--json" {
				jsonFlag = true
				break
			}
		}
		if len(args) != 4 || args[0] != "agent" || args[1] != "explain" || !jsonFlag || args[3] != "worker" {
			t.Fatalf("args=%v", args)
		}
		return `{"state":"blocked","visible_blocker":true,"matched_rule":{"id":"auth","state":"blocked"}}`, nil
	}
	got, err := ExplainAgent("worker")
	if err != nil || got.State != "blocked" || !got.VisibleBlocker || got.MatchedRule.ID != "auth" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
