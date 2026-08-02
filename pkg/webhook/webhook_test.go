package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type errorReader struct {
	err error
}

func (r *errorReader) Read(p []byte) (int, error) { return 0, r.err }
func (r *errorReader) Close() error               { return nil }

func TestReceiver_ServeHTTP(t *testing.T) {
	secret := "mysecret123"
	receiver := NewReceiver(secret)

	var dispatchedRef string
	receiver.RegisterHandler(func(event *WebhookEvent) error {
		dispatchedRef = event.TaskRef
		return nil
	})

	payload := []byte(`{
		"provider": "kaneo",
		"type": "task.created",
		"task_ref": "FAC-42",
		"project_id": "b939c5jzixruza3vvywrg1hs",
		"payload": {"title": "Webhook Test"}
	}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(payload))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()

	receiver.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}

	if dispatchedRef != "FAC-42" {
		t.Errorf("expected dispatched task_ref 'FAC-42', got '%s'", dispatchedRef)
	}
}

func TestReceiver_ServeHTTP_NoSecret(t *testing.T) {
	receiver := NewReceiver("")
	payload := []byte(`{"provider":"kaneo"}`)
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(payload))
	w := httptest.NewRecorder()
	receiver.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 OK when no secret, got %d", w.Code)
	}
}

func TestReceiver_ServeHTTP_InvalidSignature(t *testing.T) {
	receiver := NewReceiver("mysecret")
	payload := []byte(`{"provider":"kaneo"}`)
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(payload))
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	w := httptest.NewRecorder()
	receiver.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for invalid signature, got %d", w.Code)
	}
}

func TestReceiver_ServeHTTP_InvalidJSON(t *testing.T) {
	receiver := NewReceiver("")
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader([]byte(`not json`)))
	w := httptest.NewRecorder()
	receiver.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid JSON, got %d", w.Code)
	}
}

func TestReceiver_ServeHTTP_KaneoSignature(t *testing.T) {
	secret := "kaneo-secret"
	receiver := NewReceiver(secret)
	payload := []byte(`{"provider":"kaneo","type":"task.updated"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(payload))
	req.Header.Set("X-Kaneo-Signature", sig)
	w := httptest.NewRecorder()
	receiver.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 OK with X-Kaneo-Signature, got %d", w.Code)
	}
}

func TestVerifyHMAC256_EmptySecret(t *testing.T) {
	if !VerifyHMAC256([]byte("data"), "", "") {
		t.Error("expected VerifyHMAC256 to return true when secret is empty")
	}
}

func TestVerifyHMAC256_Invalid(t *testing.T) {
	if VerifyHMAC256([]byte("data"), "sha256=invalid", "secret") {
		t.Errorf("expected invalid signature check to fail")
	}
}

func TestVerifyHMAC256_EmptySignatureWithSecret(t *testing.T) {
	if !VerifyHMAC256([]byte("data"), "", "secret") {
		t.Error("expected VerifyHMAC256 to return true when signature is empty but secret is set")
	}
}

func TestServeHTTP_BodyReadError(t *testing.T) {
	receiver := NewReceiver("")
	badBody := &errorReader{err: errors.New("read error")}
	req := httptest.NewRequest("POST", "/webhook", badBody)
	w := httptest.NewRecorder()
	receiver.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for body read error, got %d", w.Code)
	}
}

func TestVerifyHMAC256_NoPrefix(t *testing.T) {
	secret := "test"
	payload := []byte("data")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := hex.EncodeToString(mac.Sum(nil))
	if !VerifyHMAC256(payload, sig, secret) {
		t.Error("expected VerifyHMAC256 to handle signature without sha256= prefix")
	}
}

func TestVerifyHMAC256_WrongSignature(t *testing.T) {
	if VerifyHMAC256([]byte("data"), strings.Repeat("a", 64), "secret") {
		t.Error("expected VerifyHMAC256 to return false for wrong signature")
	}
}