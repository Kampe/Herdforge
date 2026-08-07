//go:build fac151_hermetic_integration && linux

package verifier

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var (
	compiledFAC151PublicKeyHex  string
	compiledFAC151CandidateSHA  string
	compiledFAC151ContainerID   string
	compiledFAC151PIDNamespace  string
	compiledFAC151UserNamespace string
	compiledFAC151SourceDigest  string
	compiledFAC151Nonce         string
	compiledFAC151ArgvJSON      string
	compiledFAC151Repository    string
	compiledFAC151Task          string
)

func compiledFAC151Admission() error {
	if hermeticReceiptPath != "/tmp/replay/receipt.json" || hermeticReplayPath != "/tmp/replay" || hermeticSourcePath != "/tmp/build/source" || hermeticTestCount != "1" || hermeticTestTimeout != "10m" {
		return errors.New("FAC-151 fixed path or test policy drifted")
	}
	if compiledFAC151PublicKeyHex == "" || compiledFAC151CandidateSHA == "" || compiledFAC151ContainerID == "" || compiledFAC151PIDNamespace == "" || compiledFAC151UserNamespace == "" || compiledFAC151SourceDigest == "" || compiledFAC151ArgvJSON == "" || compiledFAC151Repository == "" || compiledFAC151Task == "" || compiledFAC151Nonce == "" {
		return errors.New("FAC-151 compiled admission bindings are incomplete")
	}
	publicKeyBytes, err := hex.DecodeString(compiledFAC151PublicKeyHex)
	if err != nil || len(publicKeyBytes) != ed25519.PublicKeySize {
		return errors.New("FAC-151 compiled public key is invalid")
	}
	verifier, err := NewTrustedReceiptVerifier(ed25519.PublicKey(publicKeyBytes))
	if err != nil {
		return err
	}
	receiptBytes, err := readImmutableBounded(hermeticReceiptPath, 64<<10)
	if err != nil {
		return fmt.Errorf("read fixed FAC-151 receipt: %w", err)
	}
	var receipt HermeticReceiptV1
	decoder := json.NewDecoder(bytes.NewReader(receiptBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return fmt.Errorf("decode fixed FAC-151 receipt: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("fixed FAC-151 receipt has trailing JSON")
	}
	var argv []string
	argvBytes, err := hex.DecodeString(compiledFAC151ArgvJSON)
	if err != nil || json.Unmarshal(argvBytes, &argv) != nil || len(argv) != len(fixedFAC151Argv()) {
		return errors.New("compiled FAC-151 argv is invalid")
	}
	for index, value := range fixedFAC151Argv() {
		if argv[index] != value {
			return errors.New("compiled FAC-151 argv differs from fixed allowlist invocation")
		}
	}
	namespaces, err := runtimeFAC151Namespaces()
	if err != nil {
		return err
	}
	uid, gid := os.Getuid(), os.Getgid()
	actualSourceDigest, err := sourceManifestDigestFromFilesystem(hermeticSourcePath)
	if err != nil || actualSourceDigest != compiledFAC151SourceDigest {
		return errors.New("copied FAC-151 source snapshot differs from compiled binding")
	}
	expectedGeneration := verifier.authorityKeyID()
	request := HermeticAdmissionRequest{
		Repository: compiledFAC151Repository, Task: compiledFAC151Task, CandidateSHA: compiledFAC151CandidateSHA,
		Argv: argv, ArgvDigest: digestArgv(argv), Isolation: IsolationBinding{Kind: IsolationContainer, ContainerIdentity: compiledFAC151ContainerID},
		PIDNamespaceIdentity: namespaces.PID, UserNamespaceIdentity: namespaces.User, UID: uid, GID: gid,
		NetworkMode: "none", MountPolicy: "immutable-copy-no-host-bind", SourceCopyDigest: compiledFAC151SourceDigest,
		Generation: expectedGeneration, Nonce: compiledFAC151Nonce,
	}
	if receipt.Generation != expectedGeneration || receipt.Nonce != compiledFAC151Nonce || receipt.PIDNamespaceIdentity != compiledFAC151PIDNamespace || receipt.UserNamespaceIdentity != compiledFAC151UserNamespace || receipt.UID != uid || receipt.GID != gid || receipt.SourceCopyDigest != actualSourceDigest {
		return errors.New("FAC-151 runtime namespace or UID/GID differs from compiled receipt")
	}
	replay, err := NewFileReplayAuthority(hermeticReplayPath, verifier)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return AdmitBeforeFixtureConstruction(context.Background(), receipt, request, verifier, replay, now, func() error { return nil })
}

func fac151TestMainAdmission() error { return compiledFAC151Admission() }

type fac151RuntimeNamespaces struct{ PID, User string }

func runtimeFAC151Namespaces() (fac151RuntimeNamespaces, error) {
	var pid, user syscall.Stat_t
	if err := unix.Stat("/proc/1/ns/pid", &pid); err != nil {
		return fac151RuntimeNamespaces{}, err
	}
	if err := unix.Stat("/proc/1/ns/user", &user); err != nil {
		return fac151RuntimeNamespaces{}, err
	}
	return fac151RuntimeNamespaces{PID: strconv.FormatUint(uint64(pid.Ino), 10), User: strconv.FormatUint(uint64(user.Ino), 10)}, nil
}
