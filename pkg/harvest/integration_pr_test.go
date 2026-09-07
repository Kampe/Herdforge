package harvest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type publicationFixture struct {
	t                    *testing.T
	publisher            IntegrationPRPublisher
	pr                   *IntegrationPR
	lookup               string
	lookupErr            error
	remote               string
	pushURL              string
	remoteErr            error
	pushes, creates      int
	loseCreate, losePush bool
	body                 string
}

func newPublicationFixture(t *testing.T) *publicationFixture {
	t.Helper()
	f := &publicationFixture{t: t}
	f.publisher.Binding = IntegrationPRBinding{Repository: "github.com/Kampe/Herdforge", Candidate: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), Branch: "integration/fac-601", Base: "main"}
	f.publisher.command = f.command
	return f
}
func (f *publicationFixture) exactPR() *IntegrationPR {
	cross := false
	return &IntegrationPR{Number: 738, State: "OPEN", URL: "https://github.com/Kampe/Herdforge/pull/738", Head: f.publisher.Binding.Head, Branch: f.publisher.Binding.Branch, Base: "main", CrossRepo: &cross}
}
func (f *publicationFixture) command(_ context.Context, name string, args []string, input string) ([]byte, error) {
	f.t.Helper()
	switch {
	case name == "git" && args[0] == "check-ref-format":
		return nil, nil
	case name == "git" && args[0] == "remote":
		if strings.Contains(strings.Join(args, " "), "--push") && f.pushURL != "" {
			return []byte(f.pushURL), nil
		}
		return []byte("git@github.com:Kampe/Herdforge.git\n"), nil
	case name == "git" && args[0] == "cat-file":
		return nil, nil
	case name == "git" && args[0] == "ls-remote":
		return []byte(f.remote), f.remoteErr
	case name == "git" && args[0] == "push":
		want := []string{"push", "--force-with-lease=refs/heads/integration/fac-601:", "origin", f.publisher.Binding.Head + ":refs/heads/integration/fac-601"}
		if !reflect.DeepEqual(args, want) {
			f.t.Fatalf("unsafe publication arguments: %q", args)
		}
		f.pushes++
		f.remote = f.publisher.Binding.Head + "\trefs/heads/integration/fac-601\n"
		if f.losePush {
			f.losePush = false
			return nil, errors.New("response lost after push")
		}
		return nil, nil
	case name == "gh" && args[0] == "pr" && args[1] == "list":
		if f.lookupErr != nil {
			return nil, f.lookupErr
		}
		if f.lookup != "" {
			return []byte(f.lookup), nil
		}
		if f.pr == nil {
			return []byte("[]"), nil
		}
		return json.Marshal([]*IntegrationPR{f.pr})
	case name == "gh" && args[0] == "pr" && args[1] == "create":
		want := []string{"pr", "create", "--repo", f.publisher.Binding.Repository, "--head", f.publisher.Binding.Branch, "--base", "main", "--title", "FAC-601", "--body-file", "-"}
		if !reflect.DeepEqual(args, want) {
			f.t.Fatalf("unexpected create arguments: %q", args)
		}
		f.creates++
		f.body = input
		f.pr = f.exactPR()
		if f.loseCreate {
			f.loseCreate = false
			return nil, errors.New("response lost after create")
		}
		return []byte(f.pr.URL), nil
	default:
		f.t.Fatalf("unexpected command %s %q", name, args)
		return nil, errors.New("unexpected command")
	}
}
func (f *publicationFixture) publish() (*IntegrationPR, error) {
	return f.publisher.Publish(context.Background(), "FAC-601", "Exact evidence\n`literal` $(not-shell)\n")
}
func TestFAC601PublicationRecoversLostResponses(t *testing.T) {
	for _, phase := range []string{"push", "create"} {
		t.Run(phase, func(t *testing.T) {
			f := newPublicationFixture(t)
			f.losePush = phase == "push"
			f.loseCreate = phase == "create"
			if _, err := f.publish(); err == nil {
				t.Fatal("lost response must remain uncertain")
			}
			pr, err := f.publish()
			if err != nil || pr == nil {
				t.Fatalf("resume: pr=%v err=%v", pr, err)
			}
			if f.pushes != 1 || f.creates != 1 {
				t.Fatalf("repeated effect: pushes=%d creates=%d", f.pushes, f.creates)
			}
			if f.body != "Exact evidence\n`literal` $(not-shell)\n" {
				t.Fatal("body was not preserved as stdin data")
			}
		})
	}
}
func TestFAC601PublicationExistingExactPRIsReadOnly(t *testing.T) {
	f := newPublicationFixture(t)
	f.pr = f.exactPR()
	if _, err := f.publish(); err != nil {
		t.Fatal(err)
	}
	if f.pushes != 0 || f.creates != 0 {
		t.Fatal("existing PR repeated publication")
	}
}
func TestFAC601PublicationUnknownNeverCreates(t *testing.T) {
	for _, raw := range []string{"null", `{"error":"timeout"}`, `[`, `[{}]`} {
		t.Run(raw, func(t *testing.T) {
			f := newPublicationFixture(t)
			f.lookup = raw
			if _, err := f.publish(); err == nil {
				t.Fatal("unknown result admitted")
			}
			if f.pushes != 0 || f.creates != 0 {
				t.Fatal("unknown result caused mutation")
			}
		})
	}
	f := newPublicationFixture(t)
	f.lookupErr = errors.New("provider deadline exceeded")
	if _, err := f.publish(); err == nil || f.pushes != 0 || f.creates != 0 {
		t.Fatal("provider failure was treated as absence")
	}
}
func TestFAC601PublicationIdentityRefusals(t *testing.T) {
	cases := map[string]func(*IntegrationPR){
		"head":               func(p *IntegrationPR) { p.Head = strings.Repeat("c", 40) },
		"base":               func(p *IntegrationPR) { p.Base = "other" },
		"branch":             func(p *IntegrationPR) { p.Branch = "integration/other" },
		"fork":               func(p *IntegrationPR) { v := true; p.CrossRepo = &v },
		"missing-fork-proof": func(p *IntegrationPR) { p.CrossRepo = nil },
		"foreign-url":        func(p *IntegrationPR) { p.URL = "https://github.com/Other/Herdforge/pull/738" },
		"closed":             func(p *IntegrationPR) { p.State = "CLOSED" },
		"contradiction":      func(p *IntegrationPR) { p.MergeCommit.OID = strings.Repeat("c", 40) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPublicationFixture(t)
			f.pr = f.exactPR()
			mutate(f.pr)
			if _, err := f.publish(); err == nil {
				t.Fatal("mismatched identity admitted")
			}
			if f.pushes != 0 || f.creates != 0 {
				t.Fatal("mismatch caused mutation")
			}
		})
	}
	f := newPublicationFixture(t)
	raw, _ := json.Marshal([]*IntegrationPR{f.exactPR(), f.exactPR()})
	f.lookup = string(raw)
	if _, err := f.publish(); err == nil {
		t.Fatal("ambiguous PR identity admitted")
	}
}
func TestFAC601PublicationRemoteRefusals(t *testing.T) {
	for _, remote := range []string{strings.Repeat("c", 40) + "\trefs/heads/integration/fac-601", "malformed", "a b c"} {
		t.Run(remote, func(t *testing.T) {
			f := newPublicationFixture(t)
			f.remote = remote
			if _, err := f.publish(); err == nil {
				t.Fatal("remote drift admitted")
			}
			if f.pushes != 0 || f.creates != 0 {
				t.Fatal("remote drift overwritten")
			}
		})
	}
	for _, pushURL := range []string{"git@github.com:Other/Herdforge.git", "git@github.com:Kampe/Herdforge.git\ngit@github.com:Other/Herdforge.git"} {
		f := newPublicationFixture(t)
		f.pushURL = pushURL
		if _, err := f.publish(); err == nil || f.pushes != 0 || f.creates != 0 {
			t.Fatal("foreign/multiple push destination admitted")
		}
	}
	f := newPublicationFixture(t)
	f.remoteErr = errors.New("timeout")
	if _, err := f.publish(); err == nil || f.pushes != 0 || f.creates != 0 {
		t.Fatal("unknown remote was treated as absent")
	}
}
func TestFAC601PublicationRequiresCreateReadback(t *testing.T) {
	f := newPublicationFixture(t)
	f.lookup = "[]"
	if _, err := f.publish(); err == nil {
		t.Fatal("create exit zero was treated as PR proof")
	}
	if f.creates != 1 {
		t.Fatal("test never reached create")
	}
}
func TestFAC601PublicationMergedReadback(t *testing.T) {
	f := newPublicationFixture(t)
	f.pr = f.exactPR()
	f.pr.State = "MERGED"
	if _, err := f.publisher.Observe(context.Background()); err == nil {
		t.Fatal("merged without commit/timestamp admitted")
	}
	f.pr.MergeCommit.OID = strings.Repeat("c", 40)
	f.pr.MergedAt = "2026-09-07T11:00:00Z"
	if _, err := f.publisher.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.pushes != 0 || f.creates != 0 {
		t.Fatal("observation mutated provider")
	}
}
