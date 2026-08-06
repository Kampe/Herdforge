package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/spin"
)

// runSpin ports bin/herd-spin: sample the live fleet and report STALL / SPIN /
// LONG. Findings are the signal, so a sample always exits 0 — a detector that
// fails the caller when it finds something makes every wrapper treat detection
// as an error.
func runSpin() {
	fs := flag.NewFlagSet("spin", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "Emit the sample as JSON")
	reset := fs.Bool("reset", false, "Wipe sample state and exit")
	tailLines := fs.Int("tail-lines", 80, "Pane tail lines to fingerprint")
	fs.Parse(os.Args[2:])

	stateFile := filepath.Join(".herd", "spin-state.json")
	if *reset {
		if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "herd spin: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("herd spin: sample state cleared")
		return
	}

	prior := map[string]spin.Sample{}
	if raw, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(raw, &prior)
	}

	agents, err := herdr.AgentList()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd spin: agent list: %v\n", err)
		os.Exit(1)
	}

	now := time.Now().UTC()
	current := map[string]spin.Sample{}
	type report struct {
		Sample   spin.Sample    `json:"sample"`
		Findings []spin.Finding `json:"findings"`
	}
	var reports []report

	for _, a := range agents {
		if a.PaneID == "" {
			continue
		}
		tail, _ := herdr.PaneRead(a.PaneID, *tailLines)
		cwd := paneCwd(a.PaneID)
		s := spin.Sample{
			PaneID:      a.PaneID,
			Name:        a.Name,
			AgentStatus: a.Status,
			Fingerprint: spin.Fingerprint(tail),
			Writer:      spin.IsWriter(a.Name, cwd),
		}
		if s.Writer && cwd != "" {
			s.Head, s.Dirty = gitSnapshot(cwd)
		}

		prev, hadPrev := prior[a.PaneID]
		// Working wall-time carries forward only while the pane stays working;
		// leaving the working state restarts the clock.
		if hadPrev && strings.EqualFold(prev.AgentStatus, "working") && strings.EqualFold(a.Status, "working") {
			s.FirstWorkingUnix = prev.FirstWorkingUnix
		} else if strings.EqualFold(a.Status, "working") {
			s.FirstWorkingUnix = now.Unix()
		}
		workingFor := time.Duration(0)
		if s.FirstWorkingUnix > 0 {
			workingFor = now.Sub(time.Unix(s.FirstWorkingUnix, 0))
		}

		var prevPtr *spin.Sample
		if hadPrev {
			prevPtr = &prev
		}
		updated, findings := spin.Classify(prevPtr, s, spin.DefaultThresholds(), workingFor)
		current[a.PaneID] = updated
		if len(findings) > 0 {
			reports = append(reports, report{Sample: updated, Findings: findings})
		}
	}

	if body, err := json.Marshal(current); err == nil {
		if err := os.MkdirAll(filepath.Dir(stateFile), 0o700); err == nil {
			os.WriteFile(stateFile, body, 0o600)
		}
	}

	if *asJSON {
		body, _ := json.MarshalIndent(reports, "", "  ")
		fmt.Println(string(body))
		return
	}
	for _, r := range reports {
		names := make([]string, 0, len(r.Findings))
		for _, f := range r.Findings {
			names = append(names, string(f))
		}
		fmt.Printf("%s pane=%s name=%s status=%s stall_hits=%d spin_hits=%d\n",
			strings.Join(names, "|"), r.Sample.PaneID, r.Sample.Name,
			r.Sample.AgentStatus, r.Sample.StallHits, r.Sample.SpinHits)
	}
	fmt.Printf("herd spin: sampled=%d findings=%d\n", len(current), len(reports))
}

// paneCwd resolves a pane's foreground working directory, or "" when unknown.
func paneCwd(paneID string) string {
	procs, err := herdr.PaneProcessInfo(paneID)
	if err != nil {
		return ""
	}
	for _, p := range procs {
		if p.Cwd != "" {
			return p.Cwd
		}
	}
	return ""
}

// gitSnapshot returns HEAD and the dirty-file count for a worktree.
func gitSnapshot(dir string) (string, int) {
	head := ""
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
		head = strings.TrimSpace(string(out))
	}
	dirty := 0
	if out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.TrimSpace(l) != "" {
				dirty++
			}
		}
	}
	return head, dirty
}
