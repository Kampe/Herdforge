package main

import (
	"flag"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func schemaFlagNames(t *testing.T) []string {
	t.Helper()
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	registerPoolReviewFlags(fs)
	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	sort.Strings(names)
	return names
}

// funcBody returns one top-level func's source, declaration through closing brace.
func funcBody(src, decl string) (string, bool) {
	start := strings.Index(src, decl)
	if start < 0 {
		return "", false
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		return "", false
	}
	return src[start : start+end], true
}

// TestEveryPoolParsingPassUsesOneSchema is the FAC-574 live-failure gate.
//
// The pool option schema existed in THREE hand-maintained copies, so --provider
// was defined in runPoolReview's operational FlagSet but rejected by the outer
// ExitOnError parser: `flag provided but not defined: -provider`, before any
// lease or tab existed. A pass that registers pool options by hand can silently
// refuse an option another pass accepts.
//
// Scoped to the three review parsers by name: unrelated commands legitimately
// have their own --provider/--pool flags and are none of this rule's business.
func TestEveryPoolParsingPassUsesOneSchema(t *testing.T) {
	passes := []struct{ file, fn string }{
		{"review_pool.go", "func runPoolReview"},
		{"review_pool.go", "func parseReviewPoolArgs"},
		{"main.go", "func parseReviewArgs"},
	}
	pool := regexp.MustCompile(`fs\.(String|Bool)\("(pool|sha|provider|model|exclude-family|pool-root|surface-root|packet-root|no-launch)"`)
	for _, p := range passes {
		src, err := os.ReadFile(p.file)
		if err != nil {
			t.Fatal(err)
		}
		body, ok := funcBody(string(src), p.fn)
		if !ok {
			t.Fatalf("%s: cannot locate %s", p.file, p.fn)
		}
		if !strings.Contains(body, "registerPoolReviewFlags") {
			t.Errorf("%s %s must register from the single pool option schema", p.file, p.fn)
		}
		for _, line := range strings.Split(body, "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") || !pool.MatchString(code) {
				continue
			}
			t.Errorf("%s %s registers a pool option by hand; call registerPoolReviewFlags instead: %s", p.file, p.fn, code)
		}
	}
	// Asserted by NAME, not by count. A bare length check fails with "expected
	// the full pool option set" and makes a legitimately added flag look like a
	// broken invariant, while saying nothing about which option is missing.
	want := map[string]bool{
		"allow-unproven-builder": true, "builder-family": true, "exclude-family": true, "model": true,
		"no-launch": true, "packet-root": true, "pool": true, "pool-root": true,
		"provider": true, "sha": true, "surface-root": true,
	}
	got := schemaFlagNames(t)
	seen := map[string]bool{}
	for _, name := range got {
		seen[name] = true
		if !want[name] {
			t.Errorf("pool option %q is not in the declared schema set", name)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("pool option %q is missing from the schema", name)
		}
	}
}

// TestPoolOptionsParseInBothArgumentOrders proves the real parsing passes accept
// the documented command line with the ref before AND after the flags. The live
// failure was order-independent, and so is this.
func TestPoolOptionsParseInBothArgumentOrders(t *testing.T) {
	full := []string{"--pool", "--sha", "8d8e1111", "--provider", "claude",
		"--exclude-family", "openai", "--model", "claude-sonnet-5",
		"--pool-root", "p", "--surface-root", "s", "--packet-root", "k", "--no-launch"}
	cases := map[string][]string{
		"ref before flags": append([]string{"wt/defi-crusader"}, full...),
		"ref after flags":  append(append([]string{}, full...), "wt/defi-crusader"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			ref, sha, err := parseReviewPoolArgs(args)
			if err != nil {
				t.Fatalf("pool parse refused the documented command line: %v", err)
			}
			if ref != "wt/defi-crusader" {
				t.Errorf("ref = %q, want wt/defi-crusader", ref)
			}
			if sha != "8d8e1111" {
				t.Errorf("sha = %q, want 8d8e1111", sha)
			}
			// The outer pass must accept the identical line. Its own FlagSet is
			// ExitOnError and would kill the test process on divergence, so
			// assert the property that matters: the same schema, parsed clean.
			fs := flag.NewFlagSet("review", flag.ContinueOnError)
			fs.Bool("spawn", false, "")
			fs.Bool("verbose", false, "")
			registerPoolReviewFlags(fs)
			if err := fs.Parse(leadingPositionalArgs(args)); err != nil {
				t.Fatalf("outer parse refused the documented command line: %v", err)
			}
		})
	}
}

// The rule that a model needs its surface must hold at parse time, before any
// git or pool work, so a malformed option is not reported only after candidate
// resolution already failed for an unrelated reason.
func TestModelRequiresProviderAtParseTime(t *testing.T) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	opts := registerPoolReviewFlags(fs)
	if err := fs.Parse([]string{"--pool", "--model", "claude-sonnet-5"}); err != nil {
		t.Fatal(err)
	}
	if err := opts.Validate(); err == nil {
		t.Fatal("--model without --provider must be refused")
	}
	fs2 := flag.NewFlagSet("probe", flag.ContinueOnError)
	opts2 := registerPoolReviewFlags(fs2)
	if err := fs2.Parse([]string{"--pool", "--provider", "claude", "--model", "claude-sonnet-5"}); err != nil {
		t.Fatal(err)
	}
	if err := opts2.Validate(); err != nil {
		t.Fatalf("--provider with --model is a valid route: %v", err)
	}
}

// TestLaunchFlagsDoNotRepeatTheHarness is the FAC-576 regression for a bug I
// introduced in FAC-574.
//
// herdr's `agent start ... -- <args>` appends these after the harness command it
// resolves from --kind, so including argv[0] ran `claude claude --model ...`,
// passing the harness name to itself as a positional. The pre-FAC-574 code
// passed flags only.
func TestLaunchFlagsDoNotRepeatTheHarness(t *testing.T) {
	r := poolReviewer{
		Kind: "claude",
		Argv: []string{"claude", "--model", "claude-sonnet-5", "--effort", "medium"},
	}
	flags := r.LaunchFlags()
	if len(flags) == 0 || flags[0] == "claude" {
		t.Fatalf("the harness command must not be repeated in the flags: %v", flags)
	}
	if flags[0] != "--model" {
		t.Errorf("flags should begin at the first real flag, got %v", flags)
	}
	// A wrapper whose argv[0] is not the kind is passed through whole rather
	// than silently truncated — dropping its first element would corrupt it.
	w := poolReviewer{Kind: "claude", Argv: []string{"wrapper", "--model", "m"}}
	if got := w.LaunchFlags(); len(got) != 3 || got[0] != "wrapper" {
		t.Errorf("a wrapper argv must pass through whole, got %v", got)
	}
	// Empty argv must not panic.
	if got := (poolReviewer{Kind: "claude"}).LaunchFlags(); len(got) != 0 {
		t.Errorf("empty argv yields no flags, got %v", got)
	}
}
