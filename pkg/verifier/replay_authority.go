package verifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const replayRecordVersion = 1
const maxReplayRecordBytes = 4096

// FileReplayAuthority is an authority-owned durable one-shot store. Root is
// supplied when the trusted launcher constructs the authority, never by a
// receipt or admission request.
type FileReplayAuthority struct {
	root        string
	authorityID string
}

type replayRecord struct {
	Version    int    `json:"version"`
	Authority  string `json:"authority"`
	Generation string `json:"generation"`
	Nonce      string `json:"nonce"`
	Payload    string `json:"payload"`
}

func NewFileReplayAuthority(root string, verifier *TrustedReceiptVerifier) (*FileReplayAuthority, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("replay authority root must be an absolute authority-owned path")
	}
	if err := verifier.validate(); err != nil {
		return nil, err
	}
	return &FileReplayAuthority{root: filepath.Clean(root), authorityID: verifier.authorityKeyID()}, nil
}

func (a *FileReplayAuthority) ConsumeOnce(ctx context.Context, token ReplayToken) (ReplayResult, error) {
	if a == nil || a.root == "" || a.authorityID == "" {
		return ReplayPersistenceFailure, errors.New("replay authority is incomplete")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ReplayPersistenceFailure, err
	}
	if token.Generation == "" || token.Nonce == "" || !validDigest(token.Payload) {
		return ReplayPersistenceFailure, errors.New("replay token is malformed")
	}
	if err := os.MkdirAll(a.root, 0o700); err != nil {
		return ReplayPersistenceFailure, fmt.Errorf("create replay authority root: %w", err)
	}
	record := replayRecord{Version: replayRecordVersion, Authority: a.authorityID, Generation: token.Generation, Nonce: token.Nonce, Payload: token.Payload}
	data, err := json.Marshal(record)
	if err != nil {
		return ReplayPersistenceFailure, fmt.Errorf("encode replay record: %w", err)
	}
	if len(data) > maxReplayRecordBytes {
		return ReplayPersistenceFailure, errors.New("replay record exceeds compiled bound")
	}
	path := filepath.Join(a.root, replayFilename(a.authorityID, token))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return ReplayPersistenceFailure, fmt.Errorf("create replay record: %w", err)
		}
		existing, readErr := readBoundedReplayRecord(path)
		if readErr != nil {
			return ReplayPersistenceFailure, readErr
		}
		if string(existing) == string(data) {
			return ReplayDuplicate, nil
		}
		return ReplayConflict, errors.New("existing replay record is mismatched")
	}
	if err := ctx.Err(); err != nil {
		return ReplayPersistenceFailure, err
	}
	n, writeErr := file.Write(data)
	if writeErr != nil || n != len(data) {
		closeErr := file.Close()
		if writeErr != nil {
			return ReplayPersistenceFailure, fmt.Errorf("write replay record: %w", writeErr)
		}
		if closeErr != nil {
			return ReplayPersistenceFailure, fmt.Errorf("write replay record short write and close: %w", closeErr)
		}
		return ReplayPersistenceFailure, errors.New("write replay record short write")
	}
	if err := file.Sync(); err != nil {
		closeErr := file.Close()
		if closeErr != nil {
			return ReplayPersistenceFailure, fmt.Errorf("sync replay record: %v; close replay record: %w", err, closeErr)
		}
		return ReplayPersistenceFailure, fmt.Errorf("sync replay record: %w", err)
	}
	if err := file.Close(); err != nil {
		return ReplayPersistenceFailure, fmt.Errorf("close replay record: %w", err)
	}
	if err := syncReplayDirectory(a.root); err != nil {
		return ReplayPersistenceFailure, err
	}
	return ReplayFresh, nil
}

func replayFilename(authority string, token ReplayToken) string {
	data, _ := json.Marshal(struct {
		Authority  string `json:"authority"`
		Generation string `json:"generation"`
		Nonce      string `json:"nonce"`
	}{authority, token.Generation, token.Nonce})
	return digestArgv([]string{string(data)})[len("sha256:"):] + ".replay"
}

func readBoundedReplayRecord(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read replay record: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxReplayRecordBytes+1))
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("read replay record: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close replay record: %w", err)
	}
	if len(data) > maxReplayRecordBytes {
		return nil, errors.New("replay record exceeds compiled bound")
	}
	var record replayRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Version != replayRecordVersion || record.Authority == "" || record.Generation == "" || record.Nonce == "" || !validDigest(record.Payload) {
		return nil, errors.New("replay record is corrupt")
	}
	canonical, err := json.Marshal(record)
	if err != nil || string(canonical) != string(data) {
		return nil, errors.New("replay record is not canonical")
	}
	return data, nil
}

func syncReplayDirectory(root string) error {
	dir, err := os.Open(root)
	if err != nil {
		return fmt.Errorf("open replay authority root for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		closeErr := dir.Close()
		if closeErr != nil {
			return fmt.Errorf("sync replay authority root: %v; close replay authority root: %w", err, closeErr)
		}
		return fmt.Errorf("sync replay authority root: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close replay authority root: %w", err)
	}
	return nil
}
