package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// FAC-598: agents create containers and abandon them, and this machine OOMs.
//
// `herd containers` audits but deliberately never removes, and its reconcile
// path only reclaims RECEIPTED containers — of which there are currently zero,
// because nothing registers ownership. So the audit correctly reported 12
// "unowned, live, no receipt" containers and could act on none of them. The
// tooling existed and nothing fed it.
//
// This reaps by EVIDENCE OF EPHEMERALITY rather than by receipt, so it works on
// containers created outside the store — which is all of them:
//
//   - testcontainers carry org.testcontainers=true and are ephemeral by
//     definition; one was found holding a Postgres box at 26-36% CPU for 39
//     hours
//   - a compose project whose working_dir lives under a pool slot, a worktree,
//     or a temp dir is a per-run stack (an abandoned bin/ci-local stack was
//     found this way)
//   - a long-exited container holds disk and an IP but does no work
//
// Protection is by explicit allowlist, never by heuristic: the shared dev stack
// is named and is never a candidate. Reaping a shared stack out from under other
// lanes would be far worse than the leak.
const (
	// containerReapProtectedProjects are compose projects this command must
	// never touch. The shared dev stack is long-lived on purpose; other lanes
	// depend on it and killing it is a fleet-wide outage, not a cleanup.
	containerReapDefaultProtected = "chainseer-e0"

	// containerReapDefaultAge keeps a live test container safe while its run is
	// plausibly still going. Below this, a "leak" is indistinguishable from work
	// in progress, and reaping it corrupts a run rather than reclaiming waste.
	containerReapDefaultAge = 45 * time.Minute
)

type reapCandidate struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Project  string `json:"project,omitempty"`
	WorkDir  string `json:"working_dir,omitempty"`
	State    string `json:"state"`
	AgeMin   int    `json:"age_minutes"`
	MemoryMB int    `json:"memory_mb,omitempty"`
	Reason   string `json:"reason"`
}

type dockerContainer struct {
	ID     string
	Name   string
	State  string
	Labels map[string]string
	Since  time.Time
}

func runContainersReap(args []string) {
	fs := flag.NewFlagSet("containers reap", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "Actually remove the candidates (default is a dry run)")
	asJSON := fs.Bool("json", false, "Machine-readable output")
	olderThan := fs.Duration("older-than", containerReapDefaultAge, "Only consider containers older than this")
	protected := fs.String("protect", containerReapDefaultProtected, "Comma-separated compose projects that must never be reaped")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	keep := map[string]bool{}
	for _, p := range strings.Split(*protected, ",") {
		if p = strings.TrimSpace(p); p != "" {
			keep[p] = true
		}
	}

	all, err := listDockerContainers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd containers reap: %v\n", err)
		os.Exit(1)
	}

	var candidates []reapCandidate
	var protectedCount int
	for _, c := range all {
		project := c.Labels["com.docker.compose.project"]
		if project != "" && keep[project] {
			protectedCount++
			continue
		}
		age := time.Since(c.Since)
		if age < *olderThan {
			continue
		}
		reason := reapReason(c, project)
		if reason == "" {
			continue
		}
		candidates = append(candidates, reapCandidate{
			ID: c.ID[:min(12, len(c.ID))], Name: c.Name, Project: project,
			WorkDir: c.Labels["com.docker.compose.project.working_dir"],
			State:   c.State, AgeMin: int(age.Minutes()), Reason: reason,
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].AgeMin > candidates[j].AgeMin })

	if *asJSON {
		out := map[string]any{
			"applied": *apply, "protected_projects": keysOf(keep),
			"protected_containers": protectedCount, "older_than": olderThan.String(),
			"candidates": candidates,
		}
		body, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(body))
	} else {
		for _, c := range candidates {
			fmt.Printf("REAP %-14s %-40s age=%dm %s\n", c.ID, c.Name, c.AgeMin, c.Reason)
		}
		fmt.Printf("containers reap: candidates=%d protected=%d (projects %s) older-than=%s applied=%v\n",
			len(candidates), protectedCount, strings.Join(keysOf(keep), ","), olderThan, *apply)
	}

	if !*apply {
		if len(candidates) > 0 {
			fmt.Println("containers reap: dry run, nothing removed (pass --apply)")
		}
		return
	}

	removed, failed := 0, 0
	for _, c := range candidates {
		if out, err := exec.Command("docker", "rm", "-f", c.ID).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "containers reap: remove %s (%s) failed: %v (%s)\n",
				c.ID, c.Name, err, strings.TrimSpace(string(out)))
			failed++
			continue
		}
		removed++
	}
	fmt.Printf("containers reap: removed=%d failed=%d\n", removed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// reapReason names why a container is reclaimable, or "" to keep it. Every
// branch requires POSITIVE evidence of ephemerality; anything unrecognised is
// kept, because an unknown long-lived container may be something an operator
// depends on and this command must not guess.
func reapReason(c dockerContainer, project string) string {
	if c.Labels["org.testcontainers"] == "true" || c.Labels["org.testcontainers.lang"] != "" {
		return "testcontainer (ephemeral by definition) abandoned by its run"
	}
	if strings.HasPrefix(strings.ToLower(c.State), "exited") {
		return "exited long ago; holds disk and an address but does no work"
	}
	if wd := c.Labels["com.docker.compose.project.working_dir"]; wd != "" && ephemeralWorkDir(wd) {
		return "compose stack rooted in an ephemeral dir (" + wd + ")"
	}
	return ""
}

// ephemeralWorkDir reports whether a compose project's working directory is a
// per-run location. A stack rooted in a pool slot or a worktree belongs to one
// verification run, so it cannot outlive that run legitimately.
func ephemeralWorkDir(dir string) bool {
	d := strings.ToLower(dir)
	for _, frag := range []string{"/.herd/pool/", managedWorktreeFrag, "/tmp/", "/private/tmp/", "/.worktrees/"} {
		if strings.Contains(d, frag) {
			return true
		}
	}
	return false
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// listDockerContainers reads every container, running or not, with the labels
// and creation time the reap decision needs.
func listDockerContainers() ([]dockerContainer, error) {
	cmd := exec.Command("docker", "ps", "-a", "--no-trunc",
		"--format", "{{.ID}}\t{{.Names}}\t{{.Status}}\t{{.CreatedAt}}\t{{.Labels}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	var list []dockerContainer
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			continue
		}
		c := dockerContainer{ID: parts[0], Name: parts[1], State: parts[2], Labels: map[string]string{}}
		// docker's CreatedAt is "2026-08-24 09:12:33 -0500 CDT"; the trailing
		// zone abbreviation is not parseable, so drop it.
		stamp := strings.TrimSpace(parts[3])
		if fields := strings.Fields(stamp); len(fields) >= 3 {
			stamp = strings.Join(fields[:3], " ")
		}
		if t, perr := time.Parse("2006-01-02 15:04:05 -0700", stamp); perr == nil {
			c.Since = t
		} else {
			// Unknown creation time means the age gate cannot be evaluated, and
			// an unevaluated gate must not authorise removal.
			continue
		}
		if len(parts) >= 5 {
			for _, kv := range strings.Split(parts[4], ",") {
				if k, v, ok := strings.Cut(kv, "="); ok {
					c.Labels[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
		}
		list = append(list, c)
	}
	return list, nil
}
