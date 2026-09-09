package usage

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sshField(v []byte) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(len(v)))
	return append(b[:], v...)
}

func ollamaFixtureKey(t *testing.T) (ollamaSigningKey, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicBlob := append(sshField([]byte("ssh-ed25519")), sshField(public)...)
	privateData := append(sshField([]byte("ssh-ed25519")), sshField(public)...)
	privateData = append(privateData, sshField(private)...)
	privateData = append(privateData, sshField([]byte("fixture"))...)
	var body bytes.Buffer
	body.WriteString("openssh-key-v1\x00")
	body.Write(sshField([]byte("none")))
	body.Write(sshField([]byte("none")))
	body.Write(sshField(nil))
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], 1)
	body.Write(count[:])
	body.Write(sshField(publicBlob))
	privateSection := append([]byte{}, []byte{0x12, 0x34, 0x56, 0x78, 0x12, 0x34, 0x56, 0x78}...)
	privateSection = append(privateSection, privateData...)
	body.Write(sshField(privateSection))
	raw := pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: body.Bytes()})
	key, err := parseOllamaOpenSSHKey(raw)
	if err != nil {
		t.Fatalf("fixture key did not parse: %v", err)
	}
	return key, raw
}

func TestOllamaPollSignsExactRequestAndMapsFractions(t *testing.T) {
	key, raw := ollamaFixtureKey(t)
	defer zeroOllamaKey(&key)
	_ = raw
	wantTime := time.Unix(1786309987, 0).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/usage" || r.URL.Query().Get("ts") != "1786309987" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		parts := strings.Split(r.Header.Get("Authorization"), ":")
		if len(parts) != 2 {
			t.Fatalf("authorization did not contain public key and signature")
		}
		blob, err := base64.StdEncoding.DecodeString(parts[0])
		if err != nil || !bytes.Equal(blob, key.blob) {
			t.Fatalf("wrong public key blob")
		}
		signature, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil || !ed25519.Verify(key.public, []byte("GET,/api/usage?ts=1786309987"), signature) {
			t.Fatalf("signature did not cover exact request URI")
		}
		_, _ = w.Write([]byte(`{"plan":"pro","limits":{"session":{"usage":0.25},"weekly":{"usage":0.75}}}`))
	}))
	defer server.Close()
	p, err := ollamaPollWithURL(server.URL, key, func() time.Time { return wantTime })
	if err != nil {
		t.Fatal(err)
	}
	if p.Plan != "pro" || p.Account == nil || p.Resources["session"].Used != 25 || p.Resources["weekly"].Remaining != 25 {
		t.Fatalf("unexpected Ollama quota mapping: %+v", p)
	}
	if p.Resources["session"].ResetsAt != "" || p.Resources["weekly"].ResetsAt != "" {
		t.Fatal("Ollama response without reset must not receive an invented reset")
	}
}

func TestOllamaPollRejectsHTTP200ErrorAndInvalidFraction(t *testing.T) {
	key, _ := ollamaFixtureKey(t)
	defer zeroOllamaKey(&key)
	for _, body := range []string{`{"error":"rate limited"}`, `{"limits":{"session":{"usage":1.1}}}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, err := ollamaPollWithURL(server.URL, key, time.Now)
		server.Close()
		if err == nil {
			t.Fatalf("invalid Ollama response %s was accepted", body)
		}
	}
}

func TestOllamaSignerAccountDoesNotMatchOpenCodeClaim(t *testing.T) {
	key, _ := ollamaFixtureKey(t)
	defer zeroOllamaKey(&key)
	ollama := identity("ollama", base64.StdEncoding.EncodeToString(key.public), "ollama-signed:/api/usage")
	opencode := identity("opencode", "same-looking-api-key", "opencode-auth:auth.json:account_id")
	if ollama.Key == opencode.Key || ollama.Provenance == opencode.Provenance {
		t.Fatalf("Ollama signer was incorrectly bound to OpenCode API-key identity: %+v / %+v", ollama, opencode)
	}
}

func TestOllamaAccountIdentityReadsOnlySameHostSigner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("OLLAMA_HOME", home)
	key, raw := ollamaFixtureKey(t)
	defer zeroOllamaKey(&key)
	if err := os.MkdirAll(filepath.Join(home), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "id_ed25519"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := providerAccountIdentity("ollama")
	want := identity("ollama", base64.StdEncoding.EncodeToString(key.public), "ollama-signed:/api/usage")
	if got == nil || got.Key != want.Key || got.Provenance != want.Provenance {
		t.Fatalf("same-host signer identity mismatch: got=%+v want=%+v", got, want)
	}
}

func TestOllamaCloudBearerContractIsCredentialBoundAndNetworkFree(t *testing.T) {
	data := t.TempDir()
	t.Setenv("OPENCODE_DATA_DIR", data)
	if err := os.WriteFile(filepath.Join(data, "auth.json"), []byte(`{"ollama-cloud":{"key":"bearer-fixture"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ollamaCloudBearerPoll()
	if err != nil || got.Status != "untracked" || got.Account == nil {
		t.Fatalf("direct bearer contract should be typed unknown without an endpoint: got=%+v err=%v", got, err)
	}
	if got.Account.Provenance != "ollama-cloud:credential-fingerprint" || strings.Contains(got.Account.Key, "bearer-fixture") {
		t.Fatalf("bearer credential was not opaquely bound: %+v", got.Account)
	}
}
