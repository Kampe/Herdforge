package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
)

type failure struct {
	Package string `json:"package"`
	Test    string `json:"test"`
}
type manifest struct {
	Version  int       `json:"version"`
	Failures []failure `json:"failures"`
}
type event struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

func main() {
	mp := flag.String("manifest", ".herd/known-failures.json", "expected-failure manifest")
	rp := flag.String("report", "", "go test -json report")
	flag.Parse()
	if *rp == "" {
		fail(errors.New("-report is required"))
	}
	want, err := readManifest(*mp)
	if err != nil {
		fail(fmt.Errorf("read manifest: %w", err))
	}
	got, err := readReport(*rp)
	if err != nil {
		fail(fmt.Errorf("read report: %w", err))
	}
	if err := compare(want, got); err != nil {
		fail(err)
	}
	fmt.Printf("known-failures: PASS (%d expected failures)\n", len(want))
}

func fail(err error) { fmt.Fprintf(os.Stderr, "known-failures: FAIL: %v\n", err); os.Exit(1) }

func readManifest(path string) ([]failure, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	return normalize(m.Failures), nil
}

func readReport(path string) ([]failure, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []failure
	s := bufio.NewScanner(f)
	for s.Scan() {
		var e event
		if json.Unmarshal(s.Bytes(), &e) == nil && e.Action == "fail" && e.Test != "" {
			out = append(out, failure{e.Package, e.Test})
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return normalize(out), nil
}

func compare(expected, actual []failure) error {
	want, got := keys(expected), keys(actual)
	if len(want) != len(got) {
		return fmt.Errorf("failure count: expected %d, got %d (expected=%v actual=%v)", len(want), len(got), want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("failure set drift: expected=%v actual=%v", want, got)
		}
	}
	return nil
}

func normalize(in []failure) []failure {
	seen := map[string]failure{}
	for _, f := range in {
		seen[f.Package+"\x00"+f.Test] = f
	}
	out := make([]failure, 0, len(seen))
	for _, f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Package == out[j].Package {
			return out[i].Test < out[j].Test
		}
		return out[i].Package < out[j].Package
	})
	return out
}

func keys(in []failure) []string {
	out := make([]string, 0, len(in))
	for _, f := range normalize(in) {
		out = append(out, f.Package+"/"+f.Test)
	}
	return out
}
