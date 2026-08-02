package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

type EventType string

const (
	EventTaskCreated   EventType = "task.created"
	EventTaskUpdated   EventType = "task.updated"
	EventCommentAdded  EventType = "comment.created"
	EventPullRequest   EventType = "pull_request.opened"
)

type WebhookEvent struct {
	Provider  string                 `json:"provider"`
	Type      EventType              `json:"type"`
	TaskRef   string                 `json:"task_ref"`
	ProjectID string                 `json:"project_id"`
	Payload   map[string]interface{} `json:"payload"`
}

type EventHandler func(event *WebhookEvent) error

type Receiver struct {
	mu       sync.RWMutex
	Secret   string
	handlers []EventHandler
}

func NewReceiver(secret string) *Receiver {
	return &Receiver{
		Secret:   secret,
		handlers: []EventHandler{},
	}
}

func (r *Receiver) RegisterHandler(h EventHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers = append(r.handlers, h)
}

func VerifyHMAC256(payload []byte, signature, secret string) bool {
	if secret == "" || signature == "" {
		return true
	}
	cleanSig := strings.TrimPrefix(signature, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedMAC := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(cleanSig), []byte(expectedMAC))
}

func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	sig := req.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		sig = req.Header.Get("X-Kaneo-Signature")
	}

	if !VerifyHMAC256(body, sig, r.Secret) {
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	r.mu.RLock()
	handlers := append([]EventHandler{}, r.handlers...)
	r.mu.RUnlock()

	for _, h := range handlers {
		_ = h(&event)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"event_dispatched"}`))
}
