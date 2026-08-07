package signerboundary

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// AdmissionRecord is a durable grant across the S/R process boundary.
// Structural JSON field matching alone is not authority.
type AdmissionRecord struct {
	TokenID      string `json:"token_id"`
	CandidateSHA string `json:"candidate_sha"`
	BaseSHA      string `json:"base_sha"`
	PatchID      string `json:"patch_id"`
	SessionID    string `json:"session_id"`
	Verdict      string `json:"verdict"`
	ExpiresUnix  int64  `json:"expires_unix,omitempty"`
	SingleUse    bool   `json:"single_use"`
	Consumed     bool   `json:"consumed,omitempty"`
}

// DurableAdmissionLedger is the cross-process FAC-145 admission channel.
// All mutations use flock(LOCK_EX) + dir fsync; shared ACL is R:SocketGID 0660
// so both R (append) and S (admit/consume) can operate.
type DurableAdmissionLedger struct {
	path string
	topo Topology
}

// AdmissionLedgerPath returns $keyDir/attest/admission.jsonl
func AdmissionLedgerPath(keyDir string) string {
	return filepath.Join(keyDir, AttestSubdir, "admission.jsonl")
}

// OpenAdmissionLedger creates/opens the ledger with R/S shared ACL.
// topo.SocketGID and topo.RequesterUID must be set for production ownership.
func OpenAdmissionLedger(path string) (*DurableAdmissionLedger, error) {
	return OpenAdmissionLedgerTopo(path, Topology{})
}

// OpenAdmissionLedgerTopo applies ownership when topo has UIDs/GID.
func OpenAdmissionLedgerTopo(path string, topo Topology) (*DurableAdmissionLedger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: empty admission ledger path", ErrProvisioning)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return nil, err
	}
	// Ensure file exists.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o660)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	// The lock sidecar must be openable by both S and R, same ACL as the ledger.
	lf, err := os.OpenFile(admissionLockPath(path), os.O_CREATE|os.O_RDWR, 0o660)
	if err != nil {
		return nil, err
	}
	_ = lf.Close()
	// Shared ACL: group-readable/writable for S+R via SocketGID.
	if topo.SocketGID > 0 {
		owner := topo.RequesterUID
		if owner <= 0 {
			owner = os.Getuid()
		}
		_ = os.Chown(path, owner, topo.SocketGID)
		_ = os.Chmod(path, 0o660)
		_ = os.Chown(admissionLockPath(path), owner, topo.SocketGID)
		_ = os.Chmod(admissionLockPath(path), 0o660)
		_ = os.Chown(dir, owner, topo.SocketGID)
		_ = os.Chmod(dir, 0o770)
	} else if g := os.Getenv(EnvSocketGID); g != "" {
		// LoadTopology may not be available in unit tests.
		if topo2, err := LoadTopology(); err == nil {
			_ = os.Chown(path, topo2.RequesterUID, topo2.SocketGID)
			_ = os.Chmod(path, 0o660)
			_ = os.Chown(admissionLockPath(path), topo2.RequesterUID, topo2.SocketGID)
			_ = os.Chmod(admissionLockPath(path), 0o660)
			topo = topo2
		}
	}
	return &DurableAdmissionLedger{path: path, topo: topo}, nil
}

// AppendGrant records a durable admission under exclusive flock.
func (l *DurableAdmissionLedger) AppendGrant(rec AdmissionRecord) error {
	if l == nil {
		return fmt.Errorf("%w: nil ledger", ErrProvisioning)
	}
	if err := validateGrant(rec); err != nil {
		return err
	}
	if !rec.SingleUse && rec.ExpiresUnix == 0 {
		rec.SingleUse = true
	}
	return l.withLock(func(f *os.File) error {
		if _, err := f.Seek(0, 2); err != nil { // end
			return err
		}
		enc := json.NewEncoder(f)
		if err := enc.Encode(rec); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		return syncDir(filepath.Dir(l.path))
	})
}

// Admit reloads under exclusive lock, matches grant, marks single-use consumed
// via rewrite under the same lock (no lost updates vs concurrent append).
func (l *DurableAdmissionLedger) Admit(req SignRequest) error {
	if l == nil {
		return fmt.Errorf("%w: admission ledger not configured", ErrVerdictNotAdmitted)
	}
	if err := DefaultAdmitReviewerVerdict(req); err != nil {
		return err
	}
	return l.withLock(func(f *os.File) error {
		recs, err := loadRecordsFrom(f)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		idx := -1
		for i := range recs {
			r := &recs[i]
			if r.Consumed {
				continue
			}
			if r.ExpiresUnix > 0 && now > r.ExpiresUnix {
				continue
			}
			if !grantMatches(r, req) {
				continue
			}
			idx = i
			break
		}
		if idx < 0 {
			return fmt.Errorf("%w: no durable admission grant for candidate/session/verdict", ErrVerdictNotAdmitted)
		}
		if recs[idx].SingleUse || recs[idx].ExpiresUnix == 0 {
			recs[idx].Consumed = true
			if err := rewriteRecordsLocked(f, l.path, recs, l.topo); err != nil {
				return err
			}
		}
		return nil
	})
}

func grantMatches(r *AdmissionRecord, req SignRequest) bool {
	return strings.TrimSpace(r.CandidateSHA) == strings.TrimSpace(req.CandidateSHA) &&
		strings.TrimSpace(r.BaseSHA) == strings.TrimSpace(req.BaseSHA) &&
		strings.TrimSpace(r.PatchID) == strings.TrimSpace(req.PatchID) &&
		strings.TrimSpace(r.SessionID) == strings.TrimSpace(req.SessionID) &&
		strings.TrimSpace(r.Verdict) == strings.TrimSpace(req.Verdict)
}

func validateGrant(rec AdmissionRecord) error {
	if strings.TrimSpace(rec.TokenID) == "" ||
		!shaHex.MatchString(strings.TrimSpace(rec.CandidateSHA)) ||
		!shaHex.MatchString(strings.TrimSpace(rec.BaseSHA)) ||
		strings.TrimSpace(rec.PatchID) == "" ||
		strings.TrimSpace(rec.SessionID) == "" ||
		!allowedVerdicts[strings.TrimSpace(rec.Verdict)] {
		return fmt.Errorf("%w: incomplete admission grant", ErrVerdictNotAdmitted)
	}
	return nil
}

// admissionLockPath is a stable sidecar whose inode is never replaced.
//
// The lock MUST NOT live on the ledger itself: rewriteRecordsLocked renames a
// temp file over the ledger, so a flock taken on the ledger is left holding an
// orphaned inode. Waiters would then read pre-rewrite records (consuming one
// single-use grant many times) and appenders would write to a detached inode
// (silently losing grants). Locking a file that is never renamed keeps the
// exclusion meaningful while the rewrite stays crash-atomic.
func admissionLockPath(ledger string) string { return ledger + ".lock" }

func (l *DurableAdmissionLedger) withLock(fn func(*os.File) error) error {
	lf, err := os.OpenFile(admissionLockPath(l.path), os.O_RDWR|os.O_CREATE, 0o660)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("%w: flock ledger: %v", ErrProvisioning, err)
	}
	defer func() { _ = unix.Flock(int(lf.Fd()), unix.LOCK_UN) }()

	// Open the ledger fresh under the lock so a rename by a previous holder
	// cannot serve stale records or absorb an append into a dead inode.
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0o660)
	if err != nil {
		return err
	}
	defer f.Close()
	return fn(f)
}

func loadRecordsFrom(f *os.File) ([]AdmissionRecord, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	var out []AdmissionRecord
	sc := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, MaxPayloadBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r AdmissionRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%w: corrupt admission ledger: %v", ErrProvisioning, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// rewriteRecordsLocked rewrites the ledger under flock, preserving mode/owner.
func rewriteRecordsLocked(f *os.File, path string, recs []AdmissionRecord, topo Topology) error {
	// Capture ownership before rewrite.
	var uid, gid int
	if fi, err := f.Stat(); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	if topo.RequesterUID > 0 {
		uid = topo.RequesterUID
	}
	if topo.SocketGID > 0 {
		gid = topo.SocketGID
	}
	tmp := path + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o660)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(tf)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			_ = tf.Close()
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := tf.Sync(); err != nil {
		_ = tf.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := tf.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if uid > 0 || gid > 0 {
		_ = os.Chown(tmp, uid, gid)
	}
	_ = os.Chmod(tmp, 0o660)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Re-apply ACL after rename (inode may change).
	if uid > 0 || gid > 0 {
		_ = os.Chown(path, uid, gid)
	}
	_ = os.Chmod(path, 0o660)
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	// Re-open f is caller's flock on old fd — still holds path via flock until unlock.
	return nil
}

// Path returns the ledger filesystem path.
func (l *DurableAdmissionLedger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}
