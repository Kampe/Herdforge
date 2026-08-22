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
	if got := schemaFlagNames(t); len(got) != 9 {
		t.Errorf("schema options = %v, expected the full pool option set", got)
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
