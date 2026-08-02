package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

func TestVerifyHMAC256_Invalid(t *testing.T) {
	if VerifyHMAC256([]byte("data"), "sha256=invalid", "secret") {
		t.Errorf("expected invalid signature check to fail")
	}
}
