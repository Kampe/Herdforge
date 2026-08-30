package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

var errPoolReviewSessionJournal = errors.New("review --pool: session binding journal invalid")

type poolReviewSessionBinding struct {
	At           time.Time `json:"at"`
	TaskRef      string    `json:"task_ref"`
	CandidateSHA string    `json:"candidate_sha"`
	LeaseID      string    `json:"lease_id"`
	AgentName    string    `json:"agent_name"`
	TabID        string    `json:"tab_id"`
	PaneID       string    `json:"pane_id"`
	Harness      string    `json:"harness"`
	SessionID    string    `json:"session_id"`
	PacketSHA256 string    `json:"packet_sha256"`
}

func freshPoolReviewConversationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("review --pool: create fresh conversation id: %w", err)
	}
	// RFC 4122 version 4 / variant bits. Claude's --session-id requires UUID.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(b[:])
	return raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32], nil
}

func bindPoolReviewConversation(kind string, flags []string, sessionID string) ([]string, error) {
	out := append([]string(nil), flags...)
	if !strings.EqualFold(strings.TrimSpace(kind), "claude") {
		return out, nil
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("review --pool: Claude requires an explicit fresh conversation id")
	}
	for _, arg := range out {
		switch strings.TrimSpace(arg) {
		case "--continue", "-c", "--resume", "-r", "--session-id":
			return nil, fmt.Errorf("review --pool: stale/ambiguous Claude conversation option %q is forbidden", arg)
		}
	}
	return append(out, "--session-id", sessionID), nil
}

func validatePoolReviewPromptBinding(binding herdr.PromptBinding, live herdr.AgentEntry) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if live.Name != binding.Name || live.TabID != binding.TabID || live.PaneID != binding.PaneID {
		return fmt.Errorf("review --pool: exact reviewer name/tab/pane drift")
	}
	if binding.Kind != "" && live.Kind != "" && !strings.EqualFold(binding.Kind, live.Kind) {
		return fmt.Errorf("review --pool: reviewer harness drift: got %s want %s", live.Kind, binding.Kind)
	}
	if strings.TrimSpace(live.Session.Value) != binding.AgentSessionID {
		return fmt.Errorf("review --pool: model conversation drift: got %q want %q", live.Session.Value, binding.AgentSessionID)
	}
	if !herdr.RealModelSessionID(live.Session.Value) {
		return fmt.Errorf("review --pool: live agent has no real model conversation identity")
	}
	return nil
}

func recordPoolReviewSession(root string, record poolReviewSessionBinding) error {
	if !validPoolReviewSessionRecord(record) {
		return fmt.Errorf("%w: incomplete record", errPoolReviewSessionJournal)
	}
	path := filepath.Join(root, ".herd", "review-session-bindings.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%w: %v", errPoolReviewSessionJournal, err)
	}
	existing, err := readPoolReviewSessions(path)
	if err != nil {
		return err
	}
	for _, prior := range existing {
		if prior.SessionID == record.SessionID {
			prior.At, record.At = time.Time{}, time.Time{}
			if reflect.DeepEqual(prior, record) {
				return nil
			}
			return fmt.Errorf("%w: session %s is already bound to a different review", errPoolReviewSessionJournal, record.SessionID)
		}
	}
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	b, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("%w: %v", errPoolReviewSessionJournal, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %v", errPoolReviewSessionJournal, err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("%w: %v", errPoolReviewSessionJournal, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("%w: %v", errPoolReviewSessionJournal, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: %v", errPoolReviewSessionJournal, err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("%w: %v", errPoolReviewSessionJournal, err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("%w: %v", errPoolReviewSessionJournal, err)
	}
	_ = dir.Close()
	readback, err := readPoolReviewSessions(path)
	if err != nil || len(readback) != len(existing)+1 {
		return fmt.Errorf("%w: append readback failed: %v", errPoolReviewSessionJournal, err)
	}
	got := readback[len(readback)-1]
	got.At, record.At = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, record) {
		return fmt.Errorf("%w: exact append readback mismatch", errPoolReviewSessionJournal)
	}
	return nil
}

func readPoolReviewSessions(path string) ([]poolReviewSessionBinding, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errPoolReviewSessionJournal, err)
	}
	defer f.Close()
	var out []poolReviewSessionBinding
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var record poolReviewSessionBinding
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("%w: partial/corrupt line: %v", errPoolReviewSessionJournal, err)
		}
		if !validPoolReviewSessionRecord(record) {
			return nil, fmt.Errorf("%w: incomplete existing record", errPoolReviewSessionJournal)
		}
		out = append(out, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", errPoolReviewSessionJournal, err)
	}
	return out, nil
}

func validPoolReviewSessionRecord(record poolReviewSessionBinding) bool {
	return strings.TrimSpace(record.TaskRef) != "" && len(strings.TrimSpace(record.CandidateSHA)) == 40 &&
		strings.TrimSpace(record.LeaseID) != "" && strings.TrimSpace(record.AgentName) != "" &&
		strings.TrimSpace(record.TabID) != "" && strings.TrimSpace(record.PaneID) != "" &&
		strings.TrimSpace(record.Harness) != "" && herdr.RealModelSessionID(record.SessionID) && len(record.PacketSHA256) == 64
}
