package signerboundary

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotateKeyAndRevokeAuthority(t *testing.T) {
	me := os.Getuid()
	gid := os.Getgid()
	topo := Topology{SignerUID: me, RequesterUID: me + 1, BuilderUID: me + 2, SocketGID: gid}
	dir := t.TempDir()
	if err := EnsureKeyLayout(dir, topo); err != nil {
		t.Fatal(err)
	}
	// Seed an initial key so rotate has a path to replace.
	repo := t.TempDir()
	res, err := RotateKeyFull(RotateOptions{
		KeyDir: dir, Identity: "id", RepoRoot: repo, Topology: topo, Publish: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.PublicHex == "" {
		t.Fatal("empty public")
	}
	if _, err := os.Lstat(res.KeyPath); err != nil {
		t.Fatal(err)
	}
	// Write a fake attestation then revoke (no live process).
	attest := AttestationFilePath(dir)
	_ = os.MkdirAll(filepath.Dir(attest), 0o770)
	if err := os.WriteFile(attest, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sock, _ := shortSocketPath("rev")
	_ = os.WriteFile(sock+".nonces", []byte("n\n"), 0o600)
	if err := RevokeAuthority(dir, "id", sock, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(attest); err == nil {
		t.Fatal("attest should be gone")
	}
	if _, err := os.Lstat(res.KeyPath); err == nil {
		t.Fatal("key should be gone")
	}
}
