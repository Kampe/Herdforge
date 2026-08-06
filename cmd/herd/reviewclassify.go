package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Kampe/Herdforge/pkg/classify"
)

// runReviewClassify ports bin/herd-review-classify: the deterministic risk
// floor for review dispatch.
//
// It never launches an agent. It compares a branch (or exact pin) with
// origin/main, prints the evidence behind the decision, and returns the
// inferred tier plus any explicit caller floor.
func runReviewClassify() {
	fs := flag.NewFlagSet("review-classify", flag.ExitOnError)
	tier := fs.String("tier", "", "Explicit caller floor: R0|R1|R2|R3")
	pin := fs.String("pin", "", "Exact candidate SHA instead of a branch tip")
	base := fs.String("base", "origin/main", "Base revision to diff against")
	pathsCSV := fs.String("paths", "", "Fixture: comma-separated changed paths")
	insertions := fs.Int("insertions", 0, "Fixture: inserted line count")
	deletions := fs.Int("deletions", 0, "Fixture: deleted line count")
	asJSON := fs.Bool("json", false, "Emit the verdict as JSON")

	// Pull the leading positional out BEFORE parsing. Go's flag package stops
	// at the first non-flag argument, so `review-classify <branch> --tier R3`
	// silently discarded --tier (and --json, yielding prose to a machine
	// consumer). This is the same defect that was fixed in harvest-merge.
	rawArgs := os.Args[2:]
	positional := ""
	if len(rawArgs) > 0 && !strings.HasPrefix(rawArgs[0], "-") {
		positional, rawArgs = rawArgs[0], rawArgs[1:]
	}
	fs.Parse(rawArgs)
	if positional == "" {
		positional = fs.Arg(0)
	}

	explicit := classify.Tier(strings.TrimSpace(*tier))
	switch explicit {
	case "", classify.TierR0, classify.TierR1, classify.TierR2, classify.TierR3:
	default:
		fmt.Fprintf(os.Stderr, "herd review-classify: --tier must be R0, R1, R2, or R3 (got %q)\n", *tier)
		os.Exit(2)
	}

	var paths []string
	target := *pin
	if *pathsCSV != "" {
		// Fixture mode: classify a described change with no repository access,
		// so the policy itself stays testable without a branch.
		for _, p := range strings.Split(*pathsCSV, ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
		if target == "" {
			target = "(fixture)"
		}
	} else {
		branch := positional
		if branch == "" && target == "" {
			fmt.Fprintln(os.Stderr, "Usage: herd review-classify <branch> [--tier R0|R1|R2|R3] [--pin SHA] [--json]")
			fmt.Fprintln(os.Stderr, "       herd review-classify --paths CSV --insertions N --deletions N [--tier ...] [--json]")
			os.Exit(2)
		}
		if target == "" {
			target = branch
		}
		var err error
		paths, *insertions, *deletions, err = diffStat(*base, target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd review-classify: %v\n", err)
			os.Exit(2)
		}
	}

	result := classify.Classify(classify.Input{CandidateSHA: target, Paths: paths})

	// An explicit floor can only RAISE the tier. Letting a caller lower an
	// inferred tier would make the classifier advisory, and the whole point is
	// that a high-risk path cannot be talked down at dispatch time.
	effective := result.Tier
	note := ""
	if explicit != "" {
		effective = classify.Max(result.Tier, explicit)
		if effective != result.Tier {
			note = fmt.Sprintf("; explicit caller floor %s raises effective tier", explicit)
		} else {
			note = fmt.Sprintf("; explicit caller floor %s does not lower inferred %s", explicit, result.Tier)
		}
	}

	fmt.Fprintf(os.Stderr, "herd review-classify: target=%s base=%s paths=%d insertions=%d deletions=%d\n",
		shortSHA(target), *base, len(paths), *insertions, *deletions)
	fmt.Fprintf(os.Stderr, "herd review-classify: inferred=%s effective=%s explicit_floor=%s\n",
		result.Tier, effective, orNone(string(explicit)))
	for _, r := range result.Rules {
		fmt.Fprintf(os.Stderr, "herd review-classify: evidence=%s tier=%s %s\n", r.ID, r.Tier, r.Reason)
	}

	if *asJSON {
		out := struct {
			classify.Result
			EffectiveTier classify.Tier `json:"effective_tier"`
			ExplicitFloor string        `json:"explicit_floor,omitempty"`
			Base          string        `json:"base"`
			Insertions    int           `json:"insertions"`
			Deletions     int           `json:"deletions"`
			Note          string        `json:"note,omitempty"`
		}{result, effective, string(explicit), *base, *insertions, *deletions, strings.TrimPrefix(note, "; ")}
		body, _ := json.Marshal(out)
		fmt.Println(string(body))
		return
	}
	fmt.Printf("review-classify: tier=%s (inferred=%s, explicit-floor=%s)%s\n",
		effective, result.Tier, orNone(string(explicit)), note)
}

// diffStat resolves the changed paths and line counts between base and target.
func diffStat(base, target string) ([]string, int, int, error) {
	out, err := exec.Command("git", "diff", "--numstat", base+"..."+target).Output()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("git diff %s...%s: %w", base, target, err)
	}
	var paths []string
	ins, del := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			continue
		}
		if n, err := strconv.Atoi(fields[0]); err == nil {
			ins += n
		}
		if n, err := strconv.Atoi(fields[1]); err == nil {
			del += n
		}
		paths = append(paths, fields[2])
	}
	return paths, ins, del, nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
