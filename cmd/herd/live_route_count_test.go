package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// Codex default and Spark are independently metered quota pools, but they use
// the same authenticated Codex account and therefore share live concurrency.
// A Spark candidate must see a running Luna agent instead of advertising a
// phantom free slot merely because the routed models differ.
func TestLiveRouteCountUsesProviderSharedAccountOccupancyAcrossCodexPools(t *testing.T) {
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		call := strings.Join(args, " ")
		switch call {
		case "agent list":
			return `{"result":{"type":"agents","agents":[
				{"name":"codex-default","agent":"codex","agent_status":"working","pane_id":"pane-default"},
				{"name":"codex-spark","agent":"codex","agent_status":"starting","pane_id":"pane-spark"},
				{"name":"codex-idle","agent":"codex","agent_status":"idle","pane_id":"pane-idle"},
				{"name":"grok-worker","agent":"grok","agent_status":"working","pane_id":"pane-grok"}
			]}}`, nil
		case "pane process-info --pane pane-default":
			return `{"result":{"process_info":{"foreground_processes":[{"pid":101,"argv":["codex","--model","gpt-5.6-luna"]}]}}}`, nil
		case "pane process-info --pane pane-spark":
			return `{"result":{"process_info":{"foreground_processes":[{"pid":102,"argv":["codex","--model","gpt-5.3-codex-spark"]}]}}}`, nil
		default:
			return "", fmt.Errorf("unexpected herdr call %q", call)
		}
	})
	t.Cleanup(restore)

	got, err := liveRouteCount("codex", "gpt-5.3-codex-spark", "spark")
	if err != nil {
		t.Fatalf("liveRouteCount: %v", err)
	}
	if got != 2 {
		t.Fatalf("Codex shared-account occupancy = %d, want 2 across default and Spark pools", got)
	}
}
