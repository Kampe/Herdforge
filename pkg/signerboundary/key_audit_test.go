package signerboundary

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func validAuditFixture(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, SignRequest, KeyAuditStatement, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	req := SignRequest{Op: OpKeyAudit, SessionID: "audit-key-prove", Nonce: "0123456789abcdef0123456789abcdef"}
	st := KeyAuditStatement{
		Domain:    keyAuditDomain,
		Nonce:     req.Nonce,
		Path:      "pkg/signerboundary/testdata/audit-key/private/id.ed25519",
		Identity:  "id",
		OwnerUID:  9,
		Mode:      "0600",
		Nlink:     1,
		ServerPID: 42,
		ServerUID: 9,
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	signReq := req
	signReq.Payload = raw
	signReq.PayloadHex = hex.EncodeToString(raw)
	sig := ed25519.Sign(priv, signReq.Canonical())
	if err := verifyKeyAudit(pub, st.Path, st.Identity, 9, 42, req, st, sig); err != nil {
		t.Fatalf("control fixture must verify: %v", err)
	}
	return pub, priv, req, st, sig
}

func TestVerifyKeyAudit_RejectsBadSignature(t *testing.T) {
	pub, _, req, st, sig := validAuditFixture(t)
	sig[0] ^= 0xff
	if err := verifyKeyAudit(pub, st.Path, st.Identity, 9, 42, req, st, sig); err == nil {
		t.Fatal("tampered signature must fail")
	}
}

func TestVerifyKeyAudit_RejectsAlteredSignedClaims(t *testing.T) {
	pub, _, req, st, sig := validAuditFixture(t)
	st.OwnerUID = 99
	st.ServerUID = 99
	if err := verifyKeyAudit(pub, st.Path, st.Identity, 99, 42, req, st, sig); err == nil {
		t.Fatal("altered signed claims must fail signature")
	}
	if err := verifyKeyAudit(pub, st.Path, st.Identity, 99, 42, req, st, sig); err != nil && !strings.Contains(err.Error(), "signature") {
		t.Fatalf("want signature rejection after static UID match, got %v", err)
	}
}

func TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID(t *testing.T) {
	pub, _, req, st, sig := validAuditFixture(t)
	cases := []struct {
		name string
		fn   func() error
		want string
	}{
		{"nonce", func() error {
			r := req
			r.Nonce = "ffffffffffffffffffffffffffffffff"
			return verifyKeyAudit(pub, st.Path, st.Identity, 9, 42, r, st, sig)
		}, "nonce"},
		{"identity", func() error {
			return verifyKeyAudit(pub, st.Path, "wrong-id", 9, 42, req, st, sig)
		}, "identity"},
		{"path", func() error {
			return verifyKeyAudit(pub, "pkg/signerboundary/testdata/audit-key/private/other.ed25519", st.Identity, 9, 42, req, st, sig)
		}, "path"},
		{"uid", func() error {
			return verifyKeyAudit(pub, st.Path, st.Identity, 8, 42, req, st, sig)
		}, "uid"},
		{"pid", func() error {
			return verifyKeyAudit(pub, st.Path, st.Identity, 9, 7, req, st, sig)
		}, "pid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			if err == nil {
				t.Fatalf("want %s mismatch", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q in %v", tc.want, err)
			}
		})
	}
}

func TestVerifyKeyAudit_RejectsMissingPinnedKey(t *testing.T) {
	_, _, req, st, sig := validAuditFixture(t)
	t.Run("nil", func(t *testing.T) {
		if err := verifyKeyAudit(nil, st.Path, st.Identity, 9, 42, req, st, sig); err == nil {
			t.Fatal("nil published key must fail")
		}
	})
	t.Run("empty", func(t *testing.T) {
		if err := verifyKeyAudit(ed25519.PublicKey{}, st.Path, st.Identity, 9, 42, req, st, sig); err == nil {
			t.Fatal("empty published key must fail")
		}
	})
}

func TestReadBoundedAuditSeed_AcceptsHexPlusNewline(t *testing.T) {
	seed := bytes.Repeat([]byte{0xab}, ed25519.SeedSize)
	raw := []byte(hex.EncodeToString(seed) + "\n")
	got, err := readBoundedAuditSeed(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("got %q", got)
	}
}

func TestReadBoundedAuditSeed_RejectsOverflow(t *testing.T) {
	raw := bytes.Repeat([]byte{'c'}, maxAuditSeedRead+1)
	_, err := readBoundedAuditSeed(bytes.NewReader(raw))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want overflow, got %v", err)
	}
}

func TestMutation_KeyAudit_NlinkZeroNeverOK(t *testing.T) {
	pub, priv, req, st, _ := validAuditFixture(t)
	st.Nlink = 0
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	signReq := req
	signReq.Payload = raw
	signReq.PayloadHex = hex.EncodeToString(raw)
	sig := ed25519.Sign(priv, signReq.Canonical())
	if err := verifyKeyAudit(pub, st.Path, st.Identity, 9, 42, req, st, sig); err == nil {
		t.Fatal("nlink=0 must fail even with a matching signature")
	}
}
