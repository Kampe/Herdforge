package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testSecret = "mysecret123"

func newTestReceiver(t *testing.T, secret string, cfg *Config) (*Receiver, *Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "webhook.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	r, err := NewReceiver(secret, store, cfg)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	return r, store
}

func sign(secret, timestamp, deliveryID string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(canonicalPayload(timestamp, deliveryID, body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// newValidRequest builds a POST request that passes every guard for the
// given receiver secret, so individual tests only need to break one
// thing at a time.
func newValidRequest(secret, deliveryID string, body []byte) *http.Request {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderDeliveryID, deliveryID)
	req.Header.Set(HeaderSignature, sign(secret, ts, deliveryID, body))
	return req
}

func TestNewReceiver_RequiresSecret(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	if _, err := NewReceiver("", store, nil); err == nil {
		t.Error("expected NewReceiver to reject an empty secret (fail-closed)")
	}
}

func TestNewReceiver_RequiresStore(t *testing.T) {
	if _, err := NewReceiver("secret", nil, nil); err == nil {
		t.Error("expected NewReceiver to reject a nil store (fail-closed)")
	}
}

func TestServeHTTP_ValidDelivery_DispatchesHandlerOnce(t *testing.T) {
	r, store := newTestReceiver(t, testSecret, nil)

	var calls int
	var gotRef string
	r.RegisterHandler(func(event *WebhookEvent) error {
		calls++
		gotRef = event.TaskRef
		return nil
	})

	body := []byte(`{"provider":"kaneo","type":"task.created","task_ref":"FAC-42","project_id":"p1","payload":{}}`)
	req := newValidRequest(testSecret, "d1", body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Errorf("expected handler to be called exactly once, got %d", calls)
	}
	if gotRef != "FAC-42" {
		t.Errorf("expected task_ref FAC-42, got %q", gotRef)
	}

	ev, err := store.Get("d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ev == nil || ev.Status != StatusProcessed {
		t.Errorf("expected persisted event with StatusProcessed, got %+v", ev)
	}
}

func TestServeHTTP_WrongMethod(t *testing.T) {
	r, _ := newTestReceiver(t, testSecret, nil)
	var calls int
	r.RegisterHandler(func(*WebhookEvent) error { calls++; return nil })

	req := newValidRequest(testSecret, "d1", []byte(`{}`))
	req.Method = http.MethodGet
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
	if calls != 0 {
		t.Error("handler must not run for a rejected method")
	}
}

func TestServeHTTP_WrongContentType(t *testing.T) {
	r, _ := newTestReceiver(t, testSecret, nil)
	var calls int
	r.RegisterHandler(func(*WebhookEvent) error { calls++; return nil })

	req := newValidRequest(testSecret, "d1", []byte(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d", w.Code)
	}
	if calls != 0 {
		t.Error("handler must not run for a rejected content type")
	}
}

func TestServeHTTP_OversizedBody(t *testing.T) {
	r, _ := newTestReceiver(t, testSecret, &Config{MaxBodyBytes: 16})
	var calls int
	r.RegisterHandler(func(*WebhookEvent) error { calls++; return nil })

	body := []byte(`{"provider":"this payload is way over the 16 byte cap"}`)
	req := newValidRequest(testSecret, "d1", body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Code)
	}
	if calls != 0 {
		t.Error("handler must not run for an oversized body")
	}
}

func TestServeHTTP_MissingHeaders(t *testing.T) {
	cases := []struct {
		name  string
		strip string
	}{
		{"missing signature", HeaderSignature},
		{"missing timestamp", HeaderTimestamp},
		{"missing delivery id", HeaderDeliveryID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestReceiver(t, testSecret, nil)
			var calls int
			r.RegisterHandler(func(*WebhookEvent) error { calls++; return nil })

			req := newValidRequest(testSecret, "d1", []byte(`{}`))
			req.Header.Del(tc.strip)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected 401 for %s, got %d", tc.name, w.Code)
			}
			if calls != 0 {
				t.Errorf("handler must not run when %s", tc.name)
			}
		})
	}
}

func TestServeHTTP_UnsupportedAlgorithm(t *testing.T) {
	r, _ := newTestReceiver(t, testSecret, nil)
	req := newValidRequest(testSecret, "d1", []byte(`{}`))
	req.Header.Set(HeaderSignature, "sha1="+strings.Repeat("a", 40))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for an unsupported algorithm prefix, got %d", w.Code)
	}
}

func TestServeHTTP_WrongSignature(t *testing.T) {
	r, _ := newTestReceiver(t, testSecret, nil)
	req := newValidRequest(testSecret, "d1", []byte(`{}`))
	req.Header.Set(HeaderSignature, "sha256="+strings.Repeat("a", 64))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a wrong signature, got %d", w.Code)
	}
}

func TestServeHTTP_ModifiedBody(t *testing.T) {
	r, _ := newTestReceiver(t, testSecret, nil)
	req := newValidRequest(testSecret, "d1", []byte(`{"a":1}`))
	// Swap the body after signing so the signature no longer matches.
	req.Body = httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte(`{"a":2}`))).Body
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a modified body, got %d", w.Code)
	}
}

func TestServeHTTP_InvalidTimestampFormat(t *testing.T) {
	r, _ := newTestReceiver(t, testSecret, nil)
	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderDeliveryID, "d1")
	req.Header.Set(HeaderTimestamp, "not-a-number")
	req.Header.Set(HeaderSignature, sign(testSecret, "not-a-number", "d1", body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a non-numeric timestamp, got %d", w.Code)
	}
}

func TestServeHTTP_StaleTimestamp_Rejects(t *testing.T) {
	r, _ := newTestReceiver(t, testSecret, &Config{MaxClockSkew: time.Minute})
	body := []byte(`{}`)
	staleTS := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderDeliveryID, "d1")
	req.Header.Set(HeaderTimestamp, staleTS)
	req.Header.Set(HeaderSignature, sign(testSecret, staleTS, "d1", body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a stale/replayed timestamp, got %d", w.Code)
	}
}

func TestServeHTTP_FutureTimestamp_Rejects(t *testing.T) {
	r, _ := newTestReceiver(t, testSecret, &Config{MaxClockSkew: time.Minute})
	body := []byte(`{}`)
	futureTS := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderDeliveryID, "d1")
	req.Header.Set(HeaderTimestamp, futureTS)
	req.Header.Set(HeaderSignature, sign(testSecret, futureTS, "d1", body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a timestamp too far in the future, got %d", w.Code)
	}
}

func TestServeHTTP_InvalidJSON_NotPersisted(t *testing.T) {
	r, store := newTestReceiver(t, testSecret, nil)
	req := newValidRequest(testSecret, "d1", []byte(`not json`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
	ev, err := store.Get("d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ev != nil {
		t.Error("expected an unparseable delivery to never be persisted")
	}
}

func TestServeHTTP_HandlerFailure_NonRetryableNever2xx_RetrySucceeds(t *testing.T) {
	r, store := newTestReceiver(t, testSecret, nil)

	var calls int
	r.RegisterHandler(func(*WebhookEvent) error {
		calls++
		if calls == 1 {
			return errors.New("boom")
		}
		return nil
	})

	body := []byte(`{"provider":"kaneo","type":"task.created","task_ref":"FAC-1","project_id":"p1","payload":{}}`)
	req1 := newValidRequest(testSecret, "d1", body)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if w1.Code < 500 {
		t.Fatalf("expected a 5xx retryable status on handler failure, got %d", w1.Code)
	}

	ev, err := store.Get("d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ev.Status != StatusPending {
		t.Errorf("expected the event to remain pending after a handler failure, got %q", ev.Status)
	}
	if ev.Attempts != 1 {
		t.Errorf("expected attempts=1, got %d", ev.Attempts)
	}

	// Retry the identical delivery — handlers must run again (not
	// short-circuited as an already-processed duplicate) since the
	// prior attempt never completed.
	req2 := newValidRequest(testSecret, "d1", body)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected the retry to succeed with 200, got %d", w2.Code)
	}
	if calls != 2 {
		t.Errorf("expected the handler to be invoked twice (fail then retry-succeed), got %d", calls)
	}

	ev, err = store.Get("d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ev.Status != StatusProcessed {
		t.Errorf("expected the event to be processed after the retry, got %q", ev.Status)
	}
}

func TestServeHTTP_PersistenceFailure_ReturnsNon2xx_HandlerNotCalled(t *testing.T) {
	r, store := newTestReceiver(t, testSecret, nil)
	var calls int
	r.RegisterHandler(func(*WebhookEvent) error { calls++; return nil })

	// Sabotage persistence: closing the DB makes the next Record call fail.
	store.Close()

	body := []byte(`{"provider":"kaneo","type":"task.created","task_ref":"FAC-1","project_id":"p1","payload":{}}`)
	req := newValidRequest(testSecret, "d1", body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code < 500 {
		t.Fatalf("expected a 5xx status when persistence fails, got %d", w.Code)
	}
	if calls != 0 {
		t.Error("handler must not run when the event could not be durably persisted first")
	}
}

func TestServeHTTP_DuplicateValidDelivery_Idempotent(t *testing.T) {
	r, store := newTestReceiver(t, testSecret, nil)
	var calls int
	r.RegisterHandler(func(*WebhookEvent) error { calls++; return nil })

	body := []byte(`{"provider":"kaneo","type":"task.created","task_ref":"FAC-1","project_id":"p1","payload":{}}`)

	req1 := newValidRequest(testSecret, "d1", body)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected first delivery to succeed with 200, got %d", w1.Code)
	}

	req2 := newValidRequest(testSecret, "d1", body)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected duplicate delivery to be acknowledged with 200, got %d", w2.Code)
	}

	if calls != 1 {
		t.Errorf("expected the handler to run exactly once across both deliveries, got %d", calls)
	}

	ev, err := store.Get("d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ev.Attempts != 0 {
		t.Errorf("expected no failed attempts recorded for a clean duplicate, got %d", ev.Attempts)
	}
}

// TestServeHTTP_ConcurrentSameDelivery_HandlerRunsOnce is the regression
// test for the race a reviewer caught at SHA 4450fb6: Store.Record and
// the later Store.MarkProcessed were two separate, non-atomic steps, so
// two simultaneous requests for the same fresh delivery id could both
// observe the row as not-yet-processed and both dispatch handlers. The
// handler sleeps to hold its claim open long enough that concurrent
// Claim attempts from the other goroutines land while it is still
// in-flight, exercising the exact window the bug lived in. Against the
// pre-fix code this test fails with calls > 1.
func TestServeHTTP_ConcurrentSameDelivery_HandlerRunsOnce(t *testing.T) {
	r, store := newTestReceiver(t, testSecret, nil)
	var calls int32
	r.RegisterHandler(func(*WebhookEvent) error {
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond)
		return nil
	})

	body := []byte(`{"provider":"kaneo","type":"task.created","task_ref":"FAC-1","project_id":"p1","payload":{}}`)

	const n = 10
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := newValidRequest(testSecret, "d1", body)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected the handler to run exactly once across %d concurrent identical deliveries, got %d", n, got)
	}

	for _, c := range codes {
		if c != http.StatusOK && c != http.StatusServiceUnavailable {
			t.Errorf("expected only 200 (won the claim / duplicate-of-processed) or 503 (lost the claim, in flight) responses, got codes=%v", codes)
			break
		}
	}

	ev, err := store.Get("d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ev.Status != StatusProcessed {
		t.Errorf("expected the delivery to end up Processed, got %q", ev.Status)
	}
	if ev.Attempts != 0 {
		t.Errorf("expected no handler failures among the concurrent deliveries, got attempts=%d", ev.Attempts)
	}
}

func TestServeHTTP_SameDeliveryIDDifferentPayload_Conflict(t *testing.T) {
	r, _ := newTestReceiver(t, testSecret, nil)

	req1 := newValidRequest(testSecret, "d1", []byte(`{"task_ref":"FAC-1"}`))
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected first delivery to succeed with 200, got %d", w1.Code)
	}

	// A different, validly-signed payload reusing the same delivery id.
	req2 := newValidRequest(testSecret, "d1", []byte(`{"task_ref":"FAC-2"}`))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Errorf("expected 409 for a delivery id reused with a different payload, got %d", w2.Code)
	}
}

func TestVerifyHMAC256(t *testing.T) {
	secret := "test-secret"
	payload := []byte("timestamp.delivery-id.body")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	validSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	cases := []struct {
		name      string
		payload   []byte
		signature string
		secret    string
		want      bool
	}{
		{"valid", payload, validSig, secret, true},
		{"empty secret", payload, validSig, "", false},
		{"empty signature", payload, "", secret, false},
		{"missing sha256 prefix (unsupported algorithm)", payload, strings.TrimPrefix(validSig, "sha256="), secret, false},
		{"wrong algorithm prefix", payload, "sha1=" + strings.TrimPrefix(validSig, "sha256="), secret, false},
		{"malformed hex", payload, "sha256=not-hex!!", secret, false},
		{"wrong signature", payload, "sha256=" + strings.Repeat("a", 64), secret, false},
		{"modified payload", []byte("different"), validSig, secret, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VerifyHMAC256(tc.payload, tc.signature, tc.secret)
			if got != tc.want {
				t.Errorf("VerifyHMAC256(%q, %q, %q) = %v, want %v", tc.payload, tc.signature, tc.secret, got, tc.want)
			}
		})
	}
}

func FuzzVerifyHMAC256(f *testing.F) {
	f.Add([]byte("payload"), "sha256=deadbeef", "secret")
	f.Add([]byte(""), "", "")
	f.Add([]byte("x"), "sha256=", "s")
	f.Add([]byte("x"), "sha1=aaaa", "s")

	f.Fuzz(func(t *testing.T, payload []byte, signature, secret string) {
		got := VerifyHMAC256(payload, signature, secret)
		if !got {
			return
		}
		// Soundness: VerifyHMAC256 must never claim a match unless the
		// signature is truly the HMAC of payload under secret.
		if secret == "" || !strings.HasPrefix(signature, "sha256=") {
			t.Fatalf("VerifyHMAC256 accepted an invalid precondition: signature=%q secret=%q", signature, secret)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(payload)
		expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if signature != expected {
			t.Fatalf("VerifyHMAC256 accepted a non-matching signature %q, expected %q", signature, expected)
		}
	})
}

func FuzzServeHTTP(f *testing.F) {
	f.Add("d1", "1700000000", "sha256=deadbeef", []byte(`{}`), "application/json")
	f.Add("", "", "", []byte(``), "")
	f.Add("d1", "not-a-number", "sha256=abcd", []byte(`{"a":1}`), "application/json")
	f.Add("d1", "1700000000", "sha1=abcd", []byte(`not json`), "text/plain")

	f.Fuzz(func(t *testing.T, deliveryID, timestamp, signature string, body []byte, contentType string) {
		dir := t.TempDir()
		store, err := NewStore(filepath.Join(dir, "fuzz.db"))
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		defer store.Close()
		r, err := NewReceiver(testSecret, store, nil)
		if err != nil {
			t.Fatalf("NewReceiver: %v", err)
		}
		var calls int
		r.RegisterHandler(func(*WebhookEvent) error { calls++; return nil })

		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if deliveryID != "" {
			req.Header.Set(HeaderDeliveryID, deliveryID)
		}
		if timestamp != "" {
			req.Header.Set(HeaderTimestamp, timestamp)
		}
		if signature != "" {
			req.Header.Set(HeaderSignature, signature)
		}

		w := httptest.NewRecorder()
		// Must not panic on arbitrary header/body input, and — since
		// signature is not derived from testSecret — must never report
		// success.
		r.ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Fatalf("unauthenticated/malformed request unexpectedly accepted: delivery=%q ts=%q sig=%q ct=%q body=%q",
				deliveryID, timestamp, signature, contentType, body)
		}
	})
}
