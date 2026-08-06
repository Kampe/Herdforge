package launch

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/router"
)

type sharedStore struct {
	mu   sync.Mutex
	snap Snapshot
}

type failOnceStore struct {
	sharedStore
	failed bool
}

func (s *failOnceStore) CompareAndSwap(v uint64, n Snapshot) (bool, error) {
	if !s.failed {
		s.failed = true
		return false, nil
	}
	return s.sharedStore.CompareAndSwap(v, n)
}

func (s *sharedStore) Read() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSnapshot(s.snap), nil
}
func (s *sharedStore) CompareAndSwap(v uint64, n Snapshot) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap.Version != v {
		return false, nil
	}
	s.snap = cloneSnapshot(n)
	return true, nil
}
func cloneSnapshot(s Snapshot) Snapshot { s.Events = append([]Event(nil), s.Events...); return s }

func authorityRequest(t *testing.T) Request {
	r := good(t)
	r.TaskRef = "FAC-191"
	r.Repository = "github.com/Kampe/Herdforge"
	r.Lane = "worker"
	r.Name = "forge-worker"
	r.TabID = "tab-7"
	r.PaneID = "pane-3"
	r.HerdrSession = "session-9"
	r.CWD = "./.herd/worktrees/fac-191"
	return r
}
func accepted(t *testing.T, a *Authority, r Receipt) Receipt {
	r.Accepted = true
	if r.ProcessIdentity == "" {
		r.ProcessIdentity = "pid=42"
	}
	if r.StartToken == "" {
		r.StartToken = "start-42"
	}
	if err := a.Accept(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAuthorityConcurrentReservationAcrossIndependentInstances(t *testing.T) {
	s := &sharedStore{}
	a, _ := NewAuthority(s)
	b, _ := NewAuthority(s)
	r := authorityRequest(t)
	var wg sync.WaitGroup
	out := make(chan Receipt, 2)
	errs := make(chan error, 2)
	for i, a := range []*Authority{a, b} {
		wg.Add(1)
		go func(x *Authority, packet string) { defer wg.Done(); q, e := x.Reserve(r, packet); out <- q; errs <- e }(a, fmt.Sprintf("packet-%d", i))
	}
	wg.Wait()
	close(out)
	close(errs)
	var rs []Receipt
	for q := range out {
		rs = append(rs, q)
	}
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if len(rs) != 2 || rs[0].Generation == rs[1].Generation {
		t.Fatalf("CAS allocated duplicate generation: %+v", rs)
	}
	if rs[0].Generation != 1 && rs[1].Generation != 1 {
		t.Fatalf("first generation missing: %+v", rs)
	}
	if snap, _ := s.Read(); len(snap.Events) != 2 {
		t.Fatalf("successful reservations were not durably committed: %+v", snap)
	}
}

func TestAuthorityCrashRestartAndExactReplay(t *testing.T) {
	p := t.TempDir() + "/state.json"
	a, _ := NewAuthority(NewFileStore(p))
	r := authorityRequest(t)
	first, err := a.Reserve(r, "packet-1")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := a.Reserve(r, "packet-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation != replay.Generation || first.StartToken != replay.StartToken {
		t.Fatal("exact reservation replay was not idempotent")
	}
	first = accepted(t, a, first)
	r.ProcessIdentity, r.StartToken = "pid=42", "start-42"
	restarted, _ := NewAuthority(NewFileStore(p))
	ok, err := restarted.HasStarted(r, "packet-1")
	if err != nil || !ok {
		t.Fatalf("restart lost accepted authority: %v %v", ok, err)
	}
	if err := restarted.Accept(first); err != nil {
		t.Fatalf("exact acceptance replay: %v", err)
	}
	bad := first
	bad.ProcessIdentity = "pid=99"
	if err := restarted.Accept(bad); err == nil {
		t.Fatal("mismatched acceptance replay was accepted")
	}
}

func TestAuthorityReplacementSupersedesStaleAcceptedEvidence(t *testing.T) {
	a, _ := NewAuthority(&sharedStore{})
	r := authorityRequest(t)
	old := accepted(t, a, mustReserve(t, a, r, "packet-old"))
	newer := accepted(t, a, mustReserve(t, a, r, "packet-new"))
	r.ProcessIdentity, r.StartToken = "pid=42", "start-42"
	if ok, err := a.HasStarted(r, "packet-old"); err != nil || ok {
		t.Fatalf("stale accepted evidence remained resumable: %v %v", ok, err)
	}
	if ok, err := a.HasStarted(r, "packet-new"); err != nil || !ok {
		t.Fatalf("new accepted evidence not resumable: %v %v", ok, err)
	}
	if err := a.Reject(old, "process failed"); err == nil {
		t.Fatal("stale generation rejection was accepted")
	}
	if ok, err := a.HasStarted(r, "packet-new"); err != nil || !ok {
		t.Fatalf("historical rejection damaged replacement: %v %v", ok, err)
	}
	if newer.Generation != old.Generation+1 {
		t.Fatalf("generation did not advance: %d %d", old.Generation, newer.Generation)
	}
}

func TestAuthorityIncidentReplacementUsesStableFamilyKey(t *testing.T) {
	a, _ := NewAuthority(&sharedStore{})
	oldReq := authorityRequest(t)
	oldReq.LeaseGeneration, oldReq.SessionGeneration = 7, 41
	old := accepted(t, a, mustReserve(t, a, oldReq, "same-packet"))
	oldReq.LeaseGeneration, oldReq.SessionGeneration = 8, 42
	oldReq.Name, oldReq.TabID, oldReq.PaneID, oldReq.HerdrSession = "forge-worker-restarted", "tab-new", "pane-new", "session-new"
	if err := a.Reject(old, "failed generation"); err != nil {
		t.Fatal(err)
	}
	newer, err := a.Reserve(oldReq, "same-packet")
	if err != nil {
		t.Fatal(err)
	}
	newer.ProcessIdentity = "pid=43"
	newer.StartToken = "start-43"
	oldReq.ProcessIdentity, oldReq.StartToken = "pid=43", "start-43"
	if newer.Generation != 2 {
		t.Fatalf("replacement did not advance family generation: %d", newer.Generation)
	}
	if ok, _ := a.HasStarted(authorityRequest(t), "same-packet"); ok {
		t.Fatal("old accepted identity remained resumable after replacement reservation")
	}
	if ok, _ := a.HasStarted(oldReq, "same-packet"); ok {
		t.Fatal("unaccepted replacement became resumable")
	}
	accepted(t, a, newer)
	snap, _ := a.Store.Read()
	if err := validateSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Events) != 5 || snap.Events[2].Kind != "rejected" || snap.Events[4].Kind != "accepted" {
		t.Fatalf("unexpected replacement transition sequence: %+v", snap.Events)
	}
	if ok, _ := a.HasStarted(oldReq, "same-packet"); !ok {
		t.Fatal("accepted replacement was not resumable")
	}
}

func TestAuthoritySamePacketActiveBindingMismatchRefused(t *testing.T) {
	a, _ := NewAuthority(&sharedStore{})
	r := authorityRequest(t)
	if _, err := a.Reserve(r, "packet"); err != nil {
		t.Fatal(err)
	}
	fields := []func(*Request){func(x *Request) { x.Name = "other" }, func(x *Request) { x.TabID = "other" }, func(x *Request) { x.PaneID = "other" }, func(x *Request) { x.HerdrSession = "other" }, func(x *Request) { x.CWD = "./other" }, func(x *Request) { x.ProcessIdentity = "pid=other" }}
	fields = append(fields, func(x *Request) { x.Decision.Role = router.RoleForgeSmith }, func(x *Request) { x.Decision.Shape = "research" }, func(x *Request) { x.Decision.Provider = "other" }, func(x *Request) { x.Decision.Model = "other" }, func(x *Request) { x.Decision.Effort = "high" }, func(x *Request) {
		x.Decision.Argv = append([]string(nil), x.Decision.Argv...)
		x.Decision.Argv[0] = "other"
	})
	for i, mutate := range fields {
		x := r
		mutate(&x)
		if _, err := a.Reserve(x, "packet"); err == nil {
			t.Fatalf("binding mismatch %d opened a new stream", i)
		}
	}
}

func TestAuthorityConcurrentConflictingAcceptsHaveOneWinner(t *testing.T) {
	s := &sharedStore{}
	a, _ := NewAuthority(s)
	b, _ := NewAuthority(s)
	r := mustReserve(t, a, authorityRequest(t), "packet")
	one, two := r, r
	one.Accepted, two.Accepted = true, true
	one.StartToken, two.StartToken = "start-one", "start-two"
	one.ProcessIdentity, two.ProcessIdentity = "pid-one", "pid-two"
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, x := range []*Authority{a, b} {
		wg.Add(1)
		go func(i int, x *Authority) {
			defer wg.Done()
			if i == 0 {
				errs <- x.Accept(one)
			} else {
				errs <- x.Accept(two)
			}
		}(i, x)
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("conflicting acceptances had %d winners", winners)
	}
	snap, _ := s.Read()
	accepted := 0
	for _, e := range snap.Events {
		if e.Kind == "accepted" {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("conflicting acceptance appended %d authorities", accepted)
	}
}

func TestAuthoritySeparatesPrestartReservationFromExactResumeProof(t *testing.T) {
	a, _ := NewAuthority(&sharedStore{})
	req := authorityRequest(t)
	reserved := mustReserve(t, a, req, "packet")
	if reserved.ProcessIdentity != "" || reserved.StartToken != "" {
		t.Fatal("reservation fabricated post-start identity")
	}
	prestarted := req
	prestarted.ProcessIdentity = "pid-forged"
	if _, err := a.Reserve(prestarted, "other-packet"); err == nil {
		t.Fatal("reservation accepted fabricated process identity")
	}
	prestarted = req
	prestarted.StartToken = "token-forged"
	if _, err := a.Reserve(prestarted, "other-packet"); err == nil {
		t.Fatal("reservation accepted fabricated start token")
	}
	b, _ := NewAuthority(&sharedStore{})
	failed := mustReserve(t, b, req, "replacement-packet")
	if err := b.Reject(failed, "agent failed before start"); err != nil {
		t.Fatal(err)
	}
	replacement := mustReserve(t, b, req, "replacement-packet")
	if replacement.Generation != 2 {
		t.Fatalf("pre-start rejection did not advance generation: %d", replacement.Generation)
	}
	if err := a.Accept(reserved); err == nil {
		t.Fatal("acceptance without actual process proof succeeded")
	}
	if ok, _ := a.HasStarted(req, "packet"); ok {
		t.Fatal("pre-start request became resumable")
	}
	acceptedReceipt := reserved
	acceptedReceipt.ProcessIdentity, acceptedReceipt.StartToken = "pid=actual", "token-actual"
	if err := a.Accept(acceptedReceipt); err != nil {
		t.Fatal(err)
	}
	proof := req
	proof.ProcessIdentity, proof.StartToken = "pid=actual", "token-actual"
	if ok, _ := a.HasStarted(proof, "packet"); !ok {
		t.Fatal("exact accepted process proof was not resumable")
	}
	wrong := proof
	wrong.ProcessIdentity, wrong.StartToken = "pid-other", "token-other"
	if ok, _ := a.HasStarted(wrong, "packet"); ok {
		t.Fatal("mismatched process proof authorized resume")
	}
}

func TestAuthorityHasStartedRejectsEveryExactBindingMismatch(t *testing.T) {
	a, _ := NewAuthority(&sharedStore{})
	req := authorityRequest(t)
	reserved := mustReserve(t, a, req, "packet")
	acceptedReceipt := accepted(t, a, reserved)
	proof := req
	proof.ProcessIdentity, proof.StartToken = acceptedReceipt.ProcessIdentity, acceptedReceipt.StartToken
	cases := []struct {
		name   string
		mutate func(*Request)
		packet string
	}{
		{"task", func(r *Request) { r.TaskRef = "other" }, "packet"}, {"repository", func(r *Request) { r.Repository = "other" }, "packet"}, {"lane", func(r *Request) { r.Lane = "other" }, "packet"}, {"name", func(r *Request) { r.Name = "other" }, "packet"}, {"lease", func(r *Request) { r.LeaseGeneration++ }, "packet"}, {"session-generation", func(r *Request) { r.SessionGeneration++ }, "packet"}, {"role", func(r *Request) { r.Decision.Role = router.RoleForgeSmith }, "packet"}, {"shape", func(r *Request) { r.Decision.Shape = "research" }, "packet"}, {"provider", func(r *Request) { r.Decision.Provider = "other" }, "packet"}, {"model", func(r *Request) { r.Decision.Model = "other" }, "packet"}, {"effort", func(r *Request) { r.Decision.Effort = "high" }, "packet"}, {"decision-digest", func(r *Request) { r.Decision.Rationale = "other" }, "packet"}, {"argv", func(r *Request) {
			r.Decision.Argv = append([]string(nil), r.Decision.Argv...)
			r.Decision.Argv[0] = "other"
		}, "packet"}, {"tab", func(r *Request) { r.TabID = "other" }, "packet"}, {"pane", func(r *Request) { r.PaneID = "other" }, "packet"}, {"herdr-session", func(r *Request) { r.HerdrSession = "other" }, "packet"}, {"cwd", func(r *Request) { r.CWD = "./other" }, "packet"}, {"process", func(r *Request) { r.ProcessIdentity = "pid-other" }, "packet"}, {"start", func(r *Request) { r.StartToken = "token-other" }, "packet"}, {"packet", func(r *Request) {}, "other-packet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := proof
			d := *proof.Decision
			d.Argv = append([]string(nil), proof.Decision.Argv...)
			x.Decision = &d
			if ok, err := a.HasStarted(x, "packet"); err != nil || !ok {
				t.Fatalf("unmodified proof did not authorize: ok=%v err=%v", ok, err)
			}
			tc.mutate(&x)
			if ok, err := a.HasStarted(x, tc.packet); err != nil || ok {
				t.Fatalf("mismatch authorized or was not safely rejected: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestFileStoreDirectorySyncFailureIsNotCASSuccess(t *testing.T) {
	s := NewFileStore(t.TempDir() + "/state.json")
	s.dirSync = func(string) error { return fmt.Errorf("directory sync failed") }
	ok, err := s.CompareAndSwap(0, Snapshot{})
	if err == nil || ok {
		t.Fatalf("directory durability failure reported success: ok=%v err=%v", ok, err)
	}
}

func TestAuthorityRejectsSemanticCorruptionBeforeResume(t *testing.T) {
	seed := authorityRequest(t)
	reserved := mustReserve(t, func() *Authority { a, _ := NewAuthority(&sharedStore{}); return a }(), seed, "packet")
	acceptedReceipt := reserved
	acceptedReceipt.ProcessIdentity, acceptedReceipt.StartToken = "pid=actual", "token-actual"
	cases := []struct {
		name   string
		events []Event
	}{
		{"unknown-kind", []Event{{Sequence: 1, Kind: "nonsense", Receipt: reserved}}},
		{"accepted-without-reservation", []Event{{Sequence: 1, Kind: "accepted", Receipt: acceptedReceipt}}},
		{"generation-gap", []Event{{Sequence: 1, Kind: "reserved", Receipt: Receipt{TaskRef: reserved.TaskRef, Repository: reserved.Repository, Lane: reserved.Lane, Role: reserved.Role, TaskShape: reserved.TaskShape, Provider: reserved.Provider, Model: reserved.Model, Effort: reserved.Effort, DecisionDigest: reserved.DecisionDigest, Argv: reserved.Argv, Name: reserved.Name, TabID: reserved.TabID, PaneID: reserved.PaneID, HerdrSession: reserved.HerdrSession, CWD: reserved.CWD, PacketDigest: reserved.PacketDigest, Generation: 2}}}},
		{"terminal-then-accepted", []Event{{Sequence: 1, Kind: "reserved", Receipt: reserved}, {Sequence: 2, Kind: "terminal", Receipt: reserved}, {Sequence: 3, Kind: "accepted", Receipt: acceptedReceipt}}},
		{"double-acceptance", []Event{{Sequence: 1, Kind: "reserved", Receipt: reserved}, {Sequence: 2, Kind: "accepted", Receipt: acceptedReceipt}, {Sequence: 3, Kind: "accepted", Receipt: acceptedReceipt}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &sharedStore{snap: Snapshot{Version: uint64(len(tc.events)), Events: tc.events}}
			a, _ := NewAuthority(s)
			proof := seed
			proof.ProcessIdentity, proof.StartToken = "pid=actual", "token-actual"
			if ok, err := a.HasStarted(proof, "packet"); err == nil && ok {
				t.Fatal("semantic corruption authorized resume")
			}
		})
	}
}

func TestAuthorityRejectsStoredArgvTamperWithPreservedDecisionDigest(t *testing.T) {
	seed := authorityRequest(t)
	store := &sharedStore{}
	a, _ := NewAuthority(store)
	reserved := mustReserve(t, a, seed, "packet")
	acceptedReceipt := accepted(t, a, reserved)
	proof := seed
	proof.ProcessIdentity, proof.StartToken = acceptedReceipt.ProcessIdentity, acceptedReceipt.StartToken
	snap, _ := store.Read()
	for i := range snap.Events {
		if snap.Events[i].Kind == "reserved" || snap.Events[i].Kind == "accepted" {
			snap.Events[i].Receipt.Argv[0] = "tampered"
		}
	}
	tampered := &sharedStore{snap: snap}
	reader, _ := NewAuthority(tampered)
	if ok, err := reader.HasStarted(proof, "packet"); err == nil && ok {
		t.Fatal("stored argv tamper authorized resume")
	}
}

func TestAuthorityRejectsAcceptanceOfReservedOlderGeneration(t *testing.T) {
	a, _ := NewAuthority(&sharedStore{})
	r := authorityRequest(t)
	old := mustReserve(t, a, r, "packet-old")
	_ = mustReserve(t, a, r, "packet-new")
	old.Accepted = true
	old.StartToken = "start-42"
	if err := a.Accept(old); err == nil {
		t.Fatal("older generation became accepted after replacement reservation")
	}
}
func mustReserve(t *testing.T, a *Authority, r Request, p string) Receipt {
	q, e := a.Reserve(r, p)
	if e != nil {
		t.Fatal(e)
	}
	return q
}

func TestAuthorityRejectsCorruptOrTruncatedState(t *testing.T) {
	p := t.TempDir() + "/state.json"
	if err := os.WriteFile(p, []byte(`{"version":1`), 0600); err != nil {
		t.Fatal(err)
	}
	a, _ := NewAuthority(NewFileStore(p))
	if _, err := a.Store.Read(); err == nil {
		t.Fatal("truncated state was accepted")
	}
}

func TestAuthorityRequiresEveryBindingField(t *testing.T) {
	base := authorityRequest(t)
	cases := []struct {
		name   string
		mutate func(*Request)
	}{
		{"task", func(r *Request) { r.TaskRef = "" }}, {"repository", func(r *Request) { r.Repository = "" }}, {"lane", func(r *Request) { r.Lane = "" }}, {"tab", func(r *Request) { r.TabID = "" }}, {"pane", func(r *Request) { r.PaneID = "" }}, {"session", func(r *Request) { r.HerdrSession = "" }}, {"cwd", func(r *Request) { r.CWD = "" }}, {"decision", func(r *Request) { r.Decision = nil }}, {"packet", func(r *Request) {}}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			packet := "packet"
			if tc.name == "packet" {
				packet = ""
			} else {
				tc.mutate(&r)
			}
			a, _ := NewAuthority(&sharedStore{})
			if _, err := a.Reserve(r, packet); err == nil {
				t.Fatal("missing binding was accepted")
			}
		})
	}
}

func TestSharedStoreCASRejectsStaleWriter(t *testing.T) {
	s := &sharedStore{}
	a, _ := NewAuthority(s)
	r := authorityRequest(t)
	if _, err := a.Reserve(r, "p"); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Read()
	if ok, err := s.CompareAndSwap(0, snap); err != nil || ok {
		t.Fatalf("stale CAS accepted: %v %v", ok, err)
	}
}

func TestAuthorityRetriesCASConflict(t *testing.T) {
	s := &failOnceStore{}
	a, _ := NewAuthority(s)
	if _, err := a.Reserve(authorityRequest(t), "packet"); err != nil {
		t.Fatal(err)
	}
	if snap, _ := s.Read(); len(snap.Events) != 1 {
		t.Fatalf("CAS conflict was not retried: %+v", snap)
	}
}
