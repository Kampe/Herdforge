package sync

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// SupersedeReceipt persists an already admitted follow-up. Admission and git
// continuity are the integration gate's responsibility; this store enforces
// exact current identity, immutable history and serialized publication.
func SupersedeReceipt(repoDir string, next *CompletionReceipt, prior string) error {
	return persistReceipt(repoDir, next, prior, nil)
}

// receiptTransition is a write-ahead decision. Once durable, a retry may finish
// this exact transition, but a competing successor cannot replace it.
type receiptTransition struct {
	Prior string `json:"prior_digest"`
	Next  string `json:"next_digest"`
}

func receiptHistoryDir(repoDir, ref string) string {
	return filepath.Join(repoDir, ".herd", "receipts", "history", NormalizeRef(ref))
}

func receiptDigestValid(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && s == fmt.Sprintf("%x", b)
}

func persistReceipt(repoDir string, next *CompletionReceipt, prior string, checkpoint func(string) error) error {
	if next == nil {
		return fmt.Errorf("nil completion receipt")
	}
	if next.TaskRef == "" || filepath.Base(next.TaskRef) != next.TaskRef || next.TaskRef == "." || next.TaskRef == ".." {
		return fmt.Errorf("invalid receipt task ref")
	}
	if prior != "" && !receiptDigestValid(prior) {
		return fmt.Errorf("prior receipt digest must be exact SHA-256")
	}
	next.Seal()
	path := ReceiptPath(repoDir, next.TaskRef)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) //nolint:errcheck
	oldBytes, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var old CompletionReceipt
	exists := err == nil
	if exists {
		if err := json.Unmarshal(oldBytes, &old); err != nil {
			return fmt.Errorf("existing receipt: %w", err)
		}
		if old.Digest == "" || old.Digest != old.ComputeDigest() {
			return fmt.Errorf("existing receipt seal is invalid")
		}
	}
	nextBytes, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	nextBytes = append(nextBytes, '\n')
	if prior == "" {
		if exists {
			if old.Digest != next.Digest {
				return fmt.Errorf("receipt already holds a different receipt; refusing to overwrite")
			}
			return nil
		}
		return receiptDurableFile(path, nextBytes, false)
	}
	if !exists {
		return fmt.Errorf("prior receipt is missing")
	}
	if old.RepoID == "" || old.RepoID != next.RepoID || old.TaskRef != next.TaskRef || (old.TaskID != "" && old.TaskID != next.TaskID) {
		return fmt.Errorf("receipt repository/task identity changed")
	}
	if old.Digest != prior && old.Digest != next.Digest {
		return fmt.Errorf("prior receipt compare-and-swap conflict")
	}
	if prior == next.Digest {
		return fmt.Errorf("follow-up must have a different receipt digest")
	}
	history := receiptHistoryDir(repoDir, next.TaskRef)
	transitionPath := filepath.Join(history, prior+".transition.json")
	transitionBytes, err := json.Marshal(receiptTransition{Prior: prior, Next: next.Digest})
	if err != nil {
		return err
	}
	transitionBytes = append(transitionBytes, '\n')
	if old.Digest == next.Digest {
		// Never accept matching current contents as proof that the supplied prior
		// was its predecessor. Require the complete durable transition history.
		b, err := os.ReadFile(transitionPath)
		if err != nil || !bytes.Equal(b, transitionBytes) {
			return fmt.Errorf("receipt transition history does not match replay")
		}
		priorBytes, err := os.ReadFile(filepath.Join(history, prior+".json"))
		if err != nil {
			return err
		}
		var previous CompletionReceipt
		if err := json.Unmarshal(priorBytes, &previous); err != nil {
			return err
		}
		if previous.Digest != prior || previous.ComputeDigest() != prior || previous.RepoID != next.RepoID || previous.TaskRef != next.TaskRef {
			return fmt.Errorf("prior history receipt is invalid")
		}
		b, err = os.ReadFile(filepath.Join(history, next.Digest+".json"))
		if err != nil || !bytes.Equal(b, nextBytes) {
			return fmt.Errorf("next history receipt is invalid")
		}
		return nil
	}
	if err := os.MkdirAll(history, 0o755); err != nil {
		return err
	}
	// Persist directory entries before relying on files within them after a crash.
	if err := receiptSyncDir(filepath.Dir(history)); err != nil {
		return err
	}
	if err := receiptSyncDir(filepath.Dir(filepath.Dir(history))); err != nil {
		return err
	}
	if err := receiptDurableFile(filepath.Join(history, prior+".json"), oldBytes, true); err != nil {
		return err
	}
	if err := receiptDurableFile(filepath.Join(history, next.Digest+".json"), nextBytes, true); err != nil {
		return err
	}
	if checkpoint != nil {
		if err := checkpoint("history"); err != nil {
			return err
		}
	}
	if err := receiptDurableFile(transitionPath, transitionBytes, true); err != nil {
		return err
	}
	if checkpoint != nil {
		if err := checkpoint("decision"); err != nil {
			return err
		}
	}
	if err := receiptDurableFile(path, nextBytes, false); err != nil {
		return err
	}
	if checkpoint != nil {
		return checkpoint("published")
	}
	return nil
}

func receiptSyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Immutable files publish via link, current receipts via atomic rename. A
// partially written temporary file can never become an authoritative receipt.
func receiptDurableFile(path string, data []byte, immutable bool) error {
	if immutable {
		existing, err := os.ReadFile(path)
		if err == nil {
			if !bytes.Equal(existing, data) {
				return fmt.Errorf("immutable receipt history conflict")
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if immutable {
		err = os.Link(f.Name(), path)
	} else {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return err
	}
	return receiptSyncDir(filepath.Dir(path))
}

// LoadPriorReceipt resolves an exact current or retained historical receipt.
// It never resolves a digest by scanning another task's history.
func LoadPriorReceipt(repoDir, ref, digest string) (*CompletionReceipt, error) {
	if ref == "" || filepath.Base(ref) != ref || ref == "." || ref == ".." {
		return nil, fmt.Errorf("invalid prior receipt task ref")
	}
	if !receiptDigestValid(digest) {
		return nil, fmt.Errorf("prior receipt digest must be exact SHA-256")
	}
	r, err := LoadReceipt(ReceiptPath(repoDir, ref))
	if err != nil {
		return nil, err
	}
	if r.Digest != digest {
		r, err = LoadReceipt(filepath.Join(receiptHistoryDir(repoDir, ref), digest+".json"))
		if err != nil {
			return nil, err
		}
	}
	if r.Digest != digest || r.ComputeDigest() != digest || r.TaskRef != NormalizeRef(ref) {
		return nil, fmt.Errorf("prior receipt identity/seal mismatch")
	}
	return r, nil
}
