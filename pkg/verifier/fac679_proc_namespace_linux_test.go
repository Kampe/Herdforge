//go:build linux

package verifier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const fac679GoExecWrapperSHA256 = "bacb081a6742fd0797704135f8a6c511ad8d7ab04d800cb2e4bffb3e0885b742"

func TestFAC679OwnedExecuteUsesNamespaceRelativeProc(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("HOME is unavailable")
	}
	wrapper := filepath.Join(home, ".local", "state", "herdforge", "verification", "fac778-0f177", "tools", "herd-isolated-go-exec-user")
	data, err := os.ReadFile(wrapper)
	if err != nil {
		t.Skipf("retained WSL Go-exec wrapper unavailable: %v", err)
	}
	hash := sha256.Sum256(data)
	if got := hex.EncodeToString(hash[:]); got != fac679GoExecWrapperSHA256 {
		t.Skipf("retained WSL Go-exec wrapper hash changed: %s", got)
	}

	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte(`package main

import (
    "fmt"
    "os"
    "strconv"
    "strings"
)

func start(path string) (string, error) {
    b, err := os.ReadFile(path)
    if err != nil { return "", err }
    s := string(b)
    i := strings.LastIndex(s, ") ")
    if i < 0 { return "", fmt.Errorf("malformed stat") }
    fields := strings.Fields(s[i+2:])
    if len(fields) < 20 { return "", fmt.Errorf("short stat") }
    return fields[19], nil
}

func pidField(path string) (int, error) {
    b, err := os.ReadFile(path)
    if err != nil { return 0, err }
    fields := strings.Fields(string(b))
    if len(fields) == 0 { return 0, fmt.Errorf("empty stat") }
    return strconv.Atoi(fields[0])
}

func main() {
    pid := os.Getpid()
    statPID, err := pidField("/proc/self/stat")
    if err != nil { panic(err) }
    if statPID != pid { panic(fmt.Sprintf("os.Getpid=%d /proc/self/stat pid=%d", pid, statPID)) }
    self, err := start("/proc/self/stat")
    if err != nil { panic(err) }
    byPID, err := start("/proc/" + strconv.Itoa(pid) + "/stat")
    if err != nil { panic(err) }
    if self != byPID { panic(fmt.Sprintf("PID %d /proc stat start token mismatch: self=%s by-pid=%s", pid, self, byPID)) }
    status, err := os.ReadFile("/proc/self/status")
    if err != nil { panic(err) }
    var nspid []string
    for _, line := range strings.Split(string(status), "\n") {
        if strings.HasPrefix(line, "NSpid:") { nspid = strings.Fields(strings.TrimPrefix(line, "NSpid:")); break }
    }
    if len(nspid) != 1 || nspid[0] != strconv.Itoa(pid) { panic(fmt.Sprintf("unexpected namespace PID view: %q", nspid)) }
}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := NewVerifierArgs([]string{wrapper, "go", "run", source}).Execute(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.Passed || result.Outcome != OutcomePASS {
		t.Fatalf("owned namespace/proc canary failed: %+v", result)
	}
}
