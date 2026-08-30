package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

func TestDecodePoolRecoveryManifestIsStrict(t *testing.T) {
	req := worktree.ReviewPoolRecoveryRequest{Version: 1, TransactionID: "fac-663"}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodePoolRecoveryManifest(b); err != nil || got.TransactionID != req.TransactionID {
		t.Fatalf("decode exact manifest: got=%+v err=%v", got, err)
	}
	unknown := strings.TrimSuffix(string(b), "}") + `,"unknown":true}`
	if _, err := decodePoolRecoveryManifest([]byte(unknown)); err == nil {
		t.Fatal("unknown manifest field accepted")
	}
	if _, err := decodePoolRecoveryManifest(append(b, []byte(` {"version":1}`)...)); err == nil {
		t.Fatal("trailing manifest value accepted")
	}
}

func TestPoolRecoveryPathRelationsAreBoundarySafe(t *testing.T) {
	root := t.TempDir()
	slot := filepath.Join(root, "pool-01")
	child := filepath.Join(slot, "nested", "file")
	siblingPrefix := filepath.Join(root, "pool-010", "file")
	if !samePoolRecoveryPath(slot, filepath.Join(root, ".", "pool-01")) {
		t.Fatal("equivalent exact paths differ")
	}
	if !poolRecoveryPathWithin(slot, child) {
		t.Fatal("real child not contained")
	}
	if poolRecoveryPathWithin(slot, siblingPrefix) {
		t.Fatal("string-prefix sibling treated as contained")
	}
}
