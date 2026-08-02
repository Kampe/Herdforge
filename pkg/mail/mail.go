package mail

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Envelope struct {
	ID        string    `json:"id"`
	Sender    string    `json:"sender"`
	Recipient string    `json:"recipient"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Read      bool      `json:"read"`
	Timestamp time.Time `json:"timestamp"`
}

type Mailbox struct {
	mu       sync.RWMutex
	MailFile string
}

func NewMailbox(mailFile string) *Mailbox {
	return &Mailbox{MailFile: mailFile}
}

func (m *Mailbox) SendMessage(sender, recipient, subject, body string) (*Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	dir := filepath.Dir(m.MailFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create mail directory: %w", err)
	}

	env := &Envelope{
		ID:        fmt.Sprintf("msg-%d", time.Now().UnixNano()),
		Sender:    sender,
		Recipient: recipient,
		Subject:   subject,
		Body:      body,
		Read:      false,
		Timestamp: time.Now(),
	}

	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal mail envelope: %w", err)
	}

	f, err := os.OpenFile(m.MailFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open mail file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("failed to write mail entry: %w", err)
	}

	return env, nil
}

func (m *Mailbox) ReadInbox(recipient string) ([]*Envelope, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if _, err := os.Stat(m.MailFile); os.IsNotExist(err) {
		return nil, nil
	}

	data, err := os.ReadFile(m.MailFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read mail file: %w", err)
	}

	var res []*Envelope

	rawLines := splitLines(string(data))
	for _, l := range rawLines {
		if len(l) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal([]byte(l), &env); err == nil {
			if recipient == "" || env.Recipient == recipient || env.Recipient == "all" {
				res = append(res, &env)
			}
		}
	}

	return res, nil
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
