// Package webhook receives inbound provider webhooks over HTTP. Every
// request must carry a valid HMAC-SHA256 signature computed over a
// canonical payload that binds the delivery timestamp and delivery id
// into the signed bytes, so neither can be tampered with independently
// of the body. A request is durably persisted (see Store) before any
// handler runs and before the response is written, so a crash between
// "verified" and "acknowledged" cannot silently drop an event. The
// package is fail-closed throughout: any missing precondition, parse
// failure, persistence failure, or handler failure returns a non-2xx
// status instead of defaulting to acceptance.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type EventType string

const (
	EventTaskCreated  EventType = "task.created"
	EventTaskUpdated  EventType = "task.updated"
	EventCommentAdded EventType = "comment.created"
	EventPullRequest  EventType = "pull_request.opened"
)

// Headers a delivery must carry. All three are required; ServeHTTP
// rejects a request missing any of them before touching the store or
// any handler.
const (
	HeaderSignature  = "X-Kaneo-Signature-256"
	HeaderTimestamp  = "X-Kaneo-Timestamp"
	HeaderDeliveryID = "X-Kaneo-Delivery-Id"
)

const (
	DefaultMaxBodyBytes = 1 << 20 // 1 MiB
	DefaultMaxClockSkew = 5 * time.Minute
)

type WebhookEvent struct {
	Provider  string                 `json:"provider"`
	Type      EventType              `json:"type"`
	TaskRef   string                 `json:"task_ref"`
	ProjectID string                 `json:"project_id"`
	Payload   map[string]interface{} `json:"payload"`
}

type EventHandler func(event *WebhookEvent) error

// Config overrides Receiver defaults. A zero Config field keeps the
// default. Present mainly so tests can exercise the body-size and
// clock-skew guards without megabyte payloads or real clock waits.
type Config struct {
	MaxBodyBytes int64
	MaxClockSkew time.Duration
}

type Receiver struct {
	mu           sync.RWMutex
	secret       string
	store        *Store
	maxBodyBytes int64
	maxClockSkew time.Duration
	handlers     []EventHandler
}

// NewReceiver builds a Receiver that verifies deliveries against secret
// and durably persists them via store before dispatching handlers. It
// fails closed at construction time rather than per-request: a Receiver
// that could ever accept a request without a configured secret or store
// must not exist.
func NewReceiver(secret string, store *Store, cfg *Config) (*Receiver, error) {
	if secret == "" {
		return nil, errors.New("webhook: secret is required (fail-closed)")
	}
	if store == nil {
		return nil, errors.New("webhook: store is required (fail-closed)")
	}
	r := &Receiver{
		secret:       secret,
		store:        store,
		maxBodyBytes: DefaultMaxBodyBytes,
		maxClockSkew: DefaultMaxClockSkew,
		handlers:     []EventHandler{},
	}
	if cfg != nil {
		if cfg.MaxBodyBytes > 0 {
			r.maxBodyBytes = cfg.MaxBodyBytes
		}
		if cfg.MaxClockSkew > 0 {
			r.maxClockSkew = cfg.MaxClockSkew
		}
	}
	return r, nil
}

func (r *Receiver) RegisterHandler(h EventHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers = append(r.handlers, h)
}

// canonicalPayload binds the timestamp and delivery id into the signed
// bytes so an attacker who intercepts a valid delivery cannot replay it
// under a different (future) timestamp, or splice a captured signature
// onto a different delivery id, without invalidating the signature.
func canonicalPayload(timestamp, deliveryID string, body []byte) []byte {
	return []byte(timestamp + "." + deliveryID + "." + string(body))
}

// VerifyHMAC256 reports whether signature is a valid "sha256=<hex>"
// HMAC-SHA256 of payload under secret, compared in constant time. It
// fails closed: an empty secret, an empty signature, a signature
// missing the "sha256=" prefix (an unsupported algorithm), or malformed
// hex all return false rather than true.
func VerifyHMAC256(payload []byte, signature, secret string) bool {
	if secret == "" || signature == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	sig, err := hex.DecodeString(signature[len(prefix):])
	if err != nil || len(sig) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expected := mac.Sum(nil)
	return hmac.Equal(sig, expected)
}

func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := req.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}

	req.Body = http.MaxBytesReader(w, req.Body, r.maxBodyBytes)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
		}
		return
	}
	defer req.Body.Close()

	sig := req.Header.Get(HeaderSignature)
	timestamp := req.Header.Get(HeaderTimestamp)
	deliveryID := req.Header.Get(HeaderDeliveryID)
	if sig == "" || timestamp == "" || deliveryID == "" {
		http.Error(w, "missing signature, timestamp, or delivery id", http.StatusUnauthorized)
		return
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		http.Error(w, "invalid timestamp", http.StatusUnauthorized)
		return
	}
	skew := time.Since(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > r.maxClockSkew {
		http.Error(w, "stale or replayed timestamp", http.StatusUnauthorized)
		return
	}

	if !VerifyHMAC256(canonicalPayload(timestamp, deliveryID, body), sig, r.secret) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}

	rec, existed, err := r.store.Record(deliveryID, event.Provider, string(event.Type), event.TaskRef, event.ProjectID, string(body))
	if err != nil {
		if errors.Is(err, ErrPayloadConflict) {
			http.Error(w, "delivery id reused for a different payload", http.StatusConflict)
			return
		}
		http.Error(w, "failed to persist event", http.StatusInternalServerError)
		return
	}

	// A duplicate of an already-processed delivery (provider retry) is
	// acknowledged without re-dispatching handlers — exactly one
	// successful dispatch per delivery id. A duplicate that is still
	// pending (a prior attempt's handler failed) falls through and
	// retries handlers below.
	if existed && rec.Status == StatusProcessed {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"duplicate"}`))
		return
	}

	r.mu.RLock()
	handlers := append([]EventHandler{}, r.handlers...)
	r.mu.RUnlock()

	for _, h := range handlers {
		if herr := h(&event); herr != nil {
			_ = r.store.MarkFailed(deliveryID, herr.Error())
			http.Error(w, fmt.Sprintf("handler failed: %v", herr), http.StatusBadGateway)
			return
		}
	}

	if err := r.store.MarkProcessed(deliveryID); err != nil {
		http.Error(w, "failed to record processed event", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"event_dispatched"}`))
}
