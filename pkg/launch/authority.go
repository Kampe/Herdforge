package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Event is an immutable transition in the launch authority. Historical
// evidence is never edited: a replacement is represented by a new event.
type Event struct {
	Sequence uint64  `json:"sequence"`
	Kind     string  `json:"kind"`
	Receipt  Receipt `json:"receipt"`
}

// Snapshot is the CAS value held by Store. Implementations may be backed by
// SQLite, provided CompareAndSwap is durable and atomic across processes.
type Snapshot struct {
	Version uint64  `json:"version"`
	Events  []Event `json:"events"`
}

// Store is the only authority seam. The expected version must be compared and
// the replacement durably committed as one operation.
type Store interface {
	Read() (Snapshot, error)
	CompareAndSwap(expected uint64, next Snapshot) (bool, error)
}

// FileStore is a production-shaped durable store. The lock is separate from
// the state file so replacement can use rename without exposing partial JSON.
type FileStore struct {
	Path    string
	DirSync func(string) error
}

func NewFileStore(path string) *FileStore { return &FileStore{Path: path} }

func syncLaunchDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *FileStore) Read() (Snapshot, error) {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return Snapshot{}, errors.New("launch store path is required")
	}
	f, err := os.Open(s.Path)
	if os.IsNotExist(err) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("open launch state: %w", err)
	}
	defer f.Close()
	var snap Snapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		if err == io.EOF {
			return Snapshot{}, errors.New("corrupt launch state: truncated")
		}
		return Snapshot{}, fmt.Errorf("corrupt launch state: %w", err)
	}
	if err := validateSnapshot(snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func (s *FileStore) CompareAndSwap(expected uint64, next Snapshot) (bool, error) {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return false, errors.New("launch store path is required")
	}
	if err := validateSnapshot(next); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0755); err != nil {
		return false, err
	}
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, fmt.Errorf("open launch state lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	current, err := s.Read()
	if err != nil {
		return false, err
	}
	if current.Version != expected {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".launch-state-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	encErr := json.NewEncoder(tmp).Encode(next)
	if encErr == nil {
		encErr = tmp.Sync()
	}
	if closeErr := tmp.Close(); encErr == nil {
		encErr = closeErr
	}
	if encErr != nil {
		return false, fmt.Errorf("write launch state: %w", encErr)
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return false, fmt.Errorf("commit launch state: %w", err)
	}
	syncDir := s.DirSync
	if syncDir == nil {
		syncDir = syncLaunchDir
	}
	if err := syncDir(filepath.Dir(s.Path)); err != nil {
		return false, fmt.Errorf("sync launch state directory: %w", err)
	}
	return true, nil
}

func validateSnapshot(s Snapshot) error {
	if s.Version != uint64(len(s.Events)) {
		return errors.New("corrupt launch state: version/sequence mismatch")
	}
	type state struct {
		r    Receipt
		kind string
	}
	states := map[string]state{}
	max := map[string]int64{}
	for i, e := range s.Events {
		if e.Sequence != uint64(i+1) {
			return errors.New("corrupt launch state: invalid event sequence")
		}
		if e.Kind != "reserved" && e.Kind != "accepted" && e.Kind != "rejected" && e.Kind != "superseded" && e.Kind != "terminal" {
			return errors.New("corrupt launch state: unknown transition")
		}
		r := e.Receipt
		key := identityKey(r)
		genKey := fmt.Sprintf("%s\x00%d", key, r.Generation)
		if r.Generation <= 0 {
			return errors.New("corrupt launch state: invalid binding")
		}
		if err := validateRejection(r); err != nil {
			return errors.New("corrupt launch state: invalid binding")
		}
		st, exists := states[genKey]
		switch e.Kind {
		case "reserved":
			if exists || r.Generation != max[key]+1 || r.ProcessIdentity != "" || r.StartToken != "" {
				return errors.New("corrupt launch state: invalid reservation provenance")
			}
			states[genKey] = state{r: r, kind: e.Kind}
			max[key] = r.Generation
		case "accepted":
			if !exists || st.kind != "reserved" || !sameBinding(st.r, r) || r.ProcessIdentity == "" || r.StartToken == "" {
				return errors.New("corrupt launch state: invalid acceptance provenance")
			}
			states[genKey] = state{r: r, kind: e.Kind}
		case "rejected":
			if !exists || (st.kind != "reserved" && st.kind != "accepted") || !sameBinding(st.r, r) || st.kind == "accepted" && !sameReceipt(st.r, r) {
				return errors.New("corrupt launch state: invalid rejection provenance")
			}
			states[genKey] = state{r: r, kind: e.Kind}
		case "superseded", "terminal":
			if !exists || (st.kind != "reserved" && st.kind != "accepted") || !sameBinding(st.r, r) {
				return errors.New("corrupt launch state: invalid terminal provenance")
			}
			if e.Kind == "superseded" {
				foundNewer := false
				for k, newer := range states {
					if strings.HasPrefix(k, key+"\x00") && newer.r.Generation > r.Generation && newer.kind == "accepted" {
						foundNewer = true
					}
				}
				if !foundNewer {
					return errors.New("corrupt launch state: invalid supersession provenance")
				}
			}
			states[genKey] = state{r: r, kind: e.Kind}
		}
	}
	return nil
}

// Authority serializes allocation and all supersession transitions through
// Store. It contains no process-local state and may be recreated after a
// crash.
type Authority struct{ Store Store }

func NewAuthority(store Store) (*Authority, error) {
	if store == nil {
		return nil, errors.New("launch store is required")
	}
	return &Authority{Store: store}, nil
}

func (a *Authority) mutate(fn func(Snapshot) (Snapshot, error)) error {
	for attempt := 0; attempt < 32; attempt++ {
		current, err := a.Store.Read()
		if err != nil {
			return err
		}
		if err := validateSnapshot(current); err != nil {
			return err
		}
		next, err := fn(current)
		if err != nil {
			return err
		}
		next.Version = uint64(len(next.Events))
		ok, err := a.Store.CompareAndSwap(current.Version, next)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return errors.New("launch state contention")
}

func cloneReceipt(r Receipt) Receipt { r.Argv = clone(r.Argv); return r }

func (a *Authority) appendEvent(kind string, r Receipt) error {
	return a.mutate(func(s Snapshot) (Snapshot, error) {
		r = cloneReceipt(r)
		r.CreatedAt = r.CreatedAt.UTC()
		s.Events = append(s.Events, Event{Sequence: uint64(len(s.Events) + 1), Kind: kind, Receipt: r})
		return s, nil
	})
}

// Reserve allocates the next generation for the exact identity. Replaying an
// already-reserved packet is idempotent; a different packet cannot reuse it.
func (a *Authority) Reserve(req Request, packetDigest string) (Receipt, error) {
	if err := validateBinding(req, packetDigest); err != nil {
		return Receipt{}, err
	}
	base := receiptFor(req, packetDigest)
	base.Generation = 1
	var result Receipt
	err := a.mutate(func(s Snapshot) (Snapshot, error) {
		key := identityKey(base)
		var max int64
		var latest Receipt
		latestKind := ""
		for _, e := range s.Events {
			if identityKey(e.Receipt) != key {
				continue
			}
			if e.Receipt.Generation >= max {
				max = e.Receipt.Generation
				latest = e.Receipt
				latestKind = e.Kind
			}
		}
		if latest.PacketDigest == base.PacketDigest && !sameBinding(latest, base) && latestKind != "rejected" && latestKind != "superseded" && latestKind != "terminal" {
			return s, errors.New("launch reservation replay mismatch")
		}
		if latest.PacketDigest == base.PacketDigest && sameBinding(latest, base) && latestKind != "rejected" && latestKind != "superseded" && latestKind != "terminal" {
			result = cloneReceipt(latest)
			return s, nil
		}
		base.Generation = max + 1
		result = cloneReceipt(base)
		s.Events = append(s.Events, Event{Sequence: uint64(len(s.Events) + 1), Kind: "reserved", Receipt: base})
		return s, nil
	})
	return result, err
}

// Accept records process identity only after the process API reports started.
func (a *Authority) Accept(r Receipt) error {
	if err := validateAccepted(r); err != nil {
		return err
	}
	return a.mutate(func(s Snapshot) (Snapshot, error) {
		found := false
		for _, e := range s.Events {
			if identityKey(e.Receipt) != identityKey(r) || e.Receipt.Generation != r.Generation {
				continue
			}
			switch e.Kind {
			case "accepted":
				if sameReceipt(e.Receipt, r) {
					return s, nil
				}
				return s, errors.New("launch acceptance replay mismatch")
			case "rejected", "superseded", "terminal":
				return s, errors.New("launch generation is terminal")
			case "reserved":
				if !sameBinding(e.Receipt, r) {
					return s, errors.New("launch acceptance binding mismatch")
				}
				found = true
			}
		}
		if !found {
			return s, errors.New("launch generation was not reserved")
		}
		for _, e := range s.Events {
			if identityKey(e.Receipt) == identityKey(r) && e.Receipt.Generation > r.Generation {
				return s, errors.New("launch generation is stale")
			}
		}
		s.Events = append(s.Events, Event{Sequence: uint64(len(s.Events) + 1), Kind: "accepted", Receipt: cloneReceipt(r)})
		older := map[int64]Event{}
		for _, e := range s.Events[:len(s.Events)-1] {
			if identityKey(e.Receipt) == identityKey(r) && e.Receipt.Generation < r.Generation {
				older[e.Receipt.Generation] = e
			}
		}
		for _, old := range older {
			if old.Kind == "accepted" || old.Kind == "reserved" {
				s.Events = append(s.Events, Event{Sequence: uint64(len(s.Events) + 1), Kind: "superseded", Receipt: old.Receipt})
			}
		}
		return s, nil
	})
}

func (a *Authority) Reject(r Receipt, reason string) error {
	if err := validateRejection(r); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("launch rejection reason is required")
	}
	r.Accepted, r.Reason = false, reason
	return a.mutate(func(s Snapshot) (Snapshot, error) {
		found := false
		for _, e := range s.Events {
			if identityKey(e.Receipt) != identityKey(r) || e.Receipt.Generation != r.Generation {
				continue
			}
			if e.Kind == "rejected" {
				if sameBinding(e.Receipt, r) && e.Receipt.Reason == reason {
					return s, nil
				}
				return s, errors.New("launch rejection replay mismatch")
			}
			if e.Kind == "superseded" || e.Kind == "terminal" {
				return s, errors.New("launch generation is terminal")
			}
			if e.Kind == "reserved" || e.Kind == "accepted" {
				matches := sameBinding(e.Receipt, r)
				if e.Kind == "accepted" {
					matches = matches && sameReceipt(e.Receipt, r)
				}
				if !matches {
					return s, errors.New("launch rejection binding mismatch")
				}
				found = true
			}
		}
		if !found {
			return s, errors.New("launch generation was not reserved or accepted")
		}
		s.Events = append(s.Events, Event{Sequence: uint64(len(s.Events) + 1), Kind: "rejected", Receipt: cloneReceipt(r)})
		return s, nil
	})
}

// HasStarted accepts only the latest exact accepted nonterminal generation.
func (a *Authority) HasStarted(req Request, packetDigest string) (bool, error) {
	if err := validateReadback(req, packetDigest); err != nil {
		return false, nil
	}
	s, err := a.Store.Read()
	if err != nil {
		return false, err
	}
	if err := validateSnapshot(s); err != nil {
		return false, err
	}
	key := identityKey(receiptFor(req, packetDigest))
	var latest int64
	var accepted *Receipt
	terminal := false
	for _, e := range s.Events {
		if identityKey(e.Receipt) != key {
			continue
		}
		if e.Receipt.Generation > latest {
			latest = e.Receipt.Generation
			accepted = nil
			terminal = false
		}
		if e.Receipt.Generation != latest {
			continue
		}
		switch e.Kind {
		case "accepted":
			r := cloneReceipt(e.Receipt)
			accepted = &r
		case "rejected", "superseded", "terminal":
			terminal = true
		}
	}
	if accepted == nil || terminal || !sameRequest(*accepted, req, packetDigest) {
		return false, nil
	}
	return true, nil
}

func receiptFor(req Request, packetDigest string) Receipt {
	role, shape, provider, model, effort, digest, argv := fields(req)
	return Receipt{TaskRef: req.TaskRef, Repository: req.Repository, Lane: req.Lane, Role: role, TaskShape: shape, Provider: provider, Model: model, Effort: effort, DecisionDigest: digest, Argv: argv, Name: req.Name, TabID: req.TabID, PaneID: req.PaneID, HerdrSession: req.HerdrSession, CWD: req.CWD, PacketDigest: packetDigest, LeaseGeneration: req.LeaseGeneration, SessionGeneration: req.SessionGeneration}
}
func validateBinding(r Request, packet string) error {
	if r.Decision == nil || r.TaskRef == "" || r.Repository == "" || r.Lane == "" || r.Name == "" || r.TabID == "" || r.PaneID == "" || r.HerdrSession == "" || r.CWD == "" || r.ProcessIdentity != "" || r.StartToken != "" || strings.TrimSpace(packet) == "" {
		return errors.New("all launch binding fields are required")
	}
	return nil
}
func validateReadback(r Request, packet string) error {
	if err := validateBinding(Request{Decision: r.Decision, TaskRef: r.TaskRef, Repository: r.Repository, Lane: r.Lane, Name: r.Name, TabID: r.TabID, PaneID: r.PaneID, HerdrSession: r.HerdrSession, CWD: r.CWD, LeaseGeneration: r.LeaseGeneration, SessionGeneration: r.SessionGeneration}, packet); err != nil {
		return err
	}
	if r.ProcessIdentity == "" || r.StartToken == "" {
		return errors.New("readback requires accepted process identity and start token")
	}
	return nil
}
func validateRejection(r Receipt) error {
	if r.Generation <= 0 || r.TaskRef == "" || r.Repository == "" || r.Lane == "" || r.TabID == "" || r.PaneID == "" || r.HerdrSession == "" || r.CWD == "" || r.PacketDigest == "" || len(r.Argv) == 0 {
		return errors.New("incomplete launch receipt")
	}
	return nil
}
func validateAccepted(r Receipt) error {
	if err := validateRejection(r); err != nil {
		return err
	}
	if r.ProcessIdentity == "" || r.StartToken == "" {
		return errors.New("accepted launch requires process identity and start token")
	}
	return nil
}
func identityKey(r Receipt) string {
	return strings.Join([]string{r.TaskRef, r.Repository, r.Lane}, "\x00")
}
func sameReceipt(a, b Receipt) bool {
	return a.TaskRef == b.TaskRef && a.Repository == b.Repository && a.Lane == b.Lane && a.Name == b.Name && a.Role == b.Role && a.TaskShape == b.TaskShape && a.Provider == b.Provider && a.Model == b.Model && a.Effort == b.Effort && a.Generation == b.Generation && a.LeaseGeneration == b.LeaseGeneration && a.SessionGeneration == b.SessionGeneration && a.TabID == b.TabID && a.PaneID == b.PaneID && a.HerdrSession == b.HerdrSession && a.CWD == b.CWD && a.ProcessIdentity == b.ProcessIdentity && a.StartToken == b.StartToken && a.PacketDigest == b.PacketDigest && a.DecisionDigest == b.DecisionDigest && equalStrings(a.Argv, b.Argv)
}
func sameBinding(a, b Receipt) bool {
	return a.TaskRef == b.TaskRef && a.Repository == b.Repository && a.Lane == b.Lane && a.Name == b.Name && a.Role == b.Role && a.TaskShape == b.TaskShape && a.Provider == b.Provider && a.Model == b.Model && a.Effort == b.Effort && a.TabID == b.TabID && a.PaneID == b.PaneID && a.HerdrSession == b.HerdrSession && a.CWD == b.CWD && a.LeaseGeneration == b.LeaseGeneration && a.SessionGeneration == b.SessionGeneration && a.PacketDigest == b.PacketDigest && a.DecisionDigest == b.DecisionDigest && equalStrings(a.Argv, b.Argv)
}
func sameRequest(r Receipt, q Request, packet string) bool {
	x := receiptFor(q, packet)
	x.Generation = r.Generation
	x.StartToken = q.StartToken
	x.ProcessIdentity = q.ProcessIdentity
	return sameReceipt(r, x)
}
