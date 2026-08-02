package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalLLMClient_GeneratePrompt(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"response": "hello from local ollama", "done": true}`))
	}))
	defer ts.Close()

	client := NewLocalLLMClient(ts.URL)
	out, err := client.GeneratePrompt(context.Background(), "llama3", "hello")
	if err != nil || out != "hello from local ollama" {
		t.Fatalf("expected 'hello from local ollama', got '%s' (err: %v)", out, err)
	}
}
