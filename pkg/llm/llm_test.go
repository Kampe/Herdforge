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

func TestGeneratePrompt_Non200Status(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := NewLocalLLMClient(ts.URL)
	_, err := client.GeneratePrompt(context.Background(), "llama3", "hello")
	if err == nil {
		t.Fatal("expected error for 500 status")
	}
}

func TestGeneratePrompt_BadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{invalid json}`))
	}))
	defer ts.Close()

	client := NewLocalLLMClient(ts.URL)
	_, err := client.GeneratePrompt(context.Background(), "llama3", "hello")
	if err == nil {
		t.Fatal("expected error for bad JSON response")
	}
}

func TestGeneratePrompt_ConnRefused(t *testing.T) {
	client := NewLocalLLMClient("http://127.0.0.1:1")
	_, err := client.GeneratePrompt(context.Background(), "llama3", "hello")
	if err == nil {
		t.Fatal("expected connection error")
	}
}
