package main

import (
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/standing"
)

// resolveSendTarget maps a standing lane role to the live Herdr agent name.
// FAC-617: shared resolution lives in herdr.ResolveStandingLaneMatches so
// herdr.Send and herd send cannot drift. This wrapper adds repo identity when
// available so LiveAgentName can hit the exact digest form.
func resolveSendTarget(target string) string {
	target = strings.TrimSpace(target)
	if target == "" || strings.Contains(target, ":") || strings.HasPrefix(target, standing.ForgePrefix) {
		return target
	}
	agents, err := herdr.AgentList()
	if err != nil || len(agents) == 0 {
		return target
	}
	for _, a := range agents {
		if a.Name == target || a.PaneID == target {
			return target
		}
	}
	repo := ""
	if cfg, cfgErr := config.LoadConfig(".herd/herd.yaml"); cfgErr == nil {
		repo = repositoryIdentityForLaunch(cfg)
	}
	matches, merr := herdr.ResolveStandingLaneMatches(target, agents, repo)
	if merr != nil || len(matches) != 1 {
		return target
	}
	return matches[0].Name
}

// sendWithResolvedTarget is the testable seam for herd send (FAC-617).
// runSend parses flags then calls this; tests drive it without os.Exit.
func sendWithResolvedTarget(target, text string, verify bool, timeout time.Duration, workspace string) (resolved string, status string, err error) {
	resolved = resolveSendTarget(target)
	if strings.TrimSpace(workspace) != "" {
		status, err = herdr.SendInWorkspace(resolved, text, verify, timeout, strings.TrimSpace(workspace))
		return resolved, status, err
	}
	status, err = herdr.Send(resolved, text, verify, timeout)
	return resolved, status, err
}
