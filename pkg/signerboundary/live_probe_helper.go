package signerboundary

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// buildLiveProbeHelper compiles a probe binary with sealed-session R sign support.
func buildLiveProbeHelper() (string, error) {
	dir, err := os.MkdirTemp("", "h169probe")
	if err != nil {
		return "", err
	}
	src := filepath.Join(dir, "probe.go")
	if err := os.WriteFile(src, []byte(liveProbeHelperSrc), 0o600); err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "probe")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build probe: %v\n%s", err, out)
	}
	_ = os.Chmod(bin, 0o755)
	return bin, nil
}

// Standalone probe — no Herd imports (cross-UID RunAs safe).
const liveProbeHelperSrc = `package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(1)
	}
	switch os.Args[1] {
	case "keyread":
		if len(os.Args) < 3 {
			os.Exit(1)
		}
		b, err := os.ReadFile(os.Args[2])
		if err == nil && len(b) > 0 {
			fmt.Println("KEY_READ_OK")
			os.Exit(0)
		}
		fmt.Printf("KEY_READ_DENIED err=%v\n", err)
		os.Exit(2)
	case "attach":
		if len(os.Args) < 3 {
			os.Exit(1)
		}
		pid, _ := strconv.Atoi(os.Args[2])
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			fmt.Printf("ATTACH_HARNESS dead %v\n", err)
			os.Exit(3)
		}
		_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, 16, uintptr(pid), 0, 0, 0, 0)
		if errno == 0 {
			fmt.Println("ATTACH_OK")
			_, _, _ = syscall.Syscall6(syscall.SYS_PTRACE, 17, uintptr(pid), 0, 0, 0, 0)
			os.Exit(0)
		}
		if errno == syscall.EPERM || errno == syscall.EACCES {
			fmt.Printf("ATTACH_DENIED %v\n", errno)
			os.Exit(2)
		}
		if err := syscall.Kill(pid, 0); err == syscall.EPERM {
			fmt.Println("ATTACH_DENIED EPERM")
			os.Exit(2)
		}
		fmt.Printf("ATTACH_HARNESS errno=%v\n", errno)
		os.Exit(3)
	case "sign":
		if len(os.Args) < 5 {
			os.Exit(1)
		}
		code := dialSign(os.Args[2], os.Args[3], os.Args[4], "session-launch-prove")
		fmt.Printf("DENIED error_code=%s\n", code)
		if code == "" {
			fmt.Println("SIG_OK")
			os.Exit(0)
		}
		os.Exit(2)
	case "sign-admitted":
		// sock keyDir ledgerPath
		if len(os.Args) < 5 {
			os.Exit(1)
		}
		sock, keyDir, ledger := os.Args[2], os.Args[3], os.Args[4]
		sk, err := loadSealed(keyDir)
		if err != nil {
			fmt.Printf("SEALED_ERR %v\n", err)
			os.Exit(1)
		}
		// Append grant (R can write ledger)
		token := fmt.Sprintf("prove-%d", time.Now().UnixNano())
		grant := map[string]any{
			"token_id": token,
			"candidate_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"base_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"patch_id": "launch-prove",
			"session_id": "session-launch-prove",
			"verdict": "APPROVED",
			"single_use": true,
		}
		f, err := os.OpenFile(ledger, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o660)
		if err != nil {
			fmt.Printf("LEDGER_ERR %v\n", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(f).Encode(grant)
		_ = f.Sync()
		_ = f.Close()
		nonce := fmt.Sprintf("n-%d", time.Now().UnixNano())
		mac := bindMAC(sk, "sign-verdict",
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"launch-prove", "APPROVED", "session-launch-prove", nonce,
			[]byte(` + "`" + `{"candidate_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","patch_id":"launch-prove","verdict":"APPROVED"}` + "`" + `))
		code := dialSign(sock, mac, nonce, "session-launch-prove")
		if code == "" {
			fmt.Println("SIG_OK")
			os.Exit(0)
		}
		fmt.Printf("DENIED error_code=%s\n", code)
		os.Exit(2)
	default:
		os.Exit(1)
	}
}

func loadSealed(keyDir string) ([]byte, error) {
	p := filepath.Join(keyDir, "attest", "session.rkey")
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return nil, fmt.Errorf("empty sealed")
	}
	return hex.DecodeString(strings.TrimSpace(sc.Text()))
}

func bindMAC(sk []byte, op, cand, base, patch, verdict, session, nonce string, payload []byte) string {
	var b strings.Builder
	b.WriteString(op)
	b.WriteByte(0)
	b.WriteString(cand)
	b.WriteByte(0)
	b.WriteString(base)
	b.WriteByte(0)
	b.WriteString(patch)
	b.WriteByte(0)
	b.WriteString(verdict)
	b.WriteByte(0)
	b.WriteString(session)
	b.WriteByte(0)
	b.WriteString(nonce)
	b.WriteByte(0)
	b.Write(payload)
	m := hmac.New(sha256.New, sk)
	_, _ = m.Write([]byte(b.String()))
	return hex.EncodeToString(m.Sum(nil))
}

func dialSign(sock, mac, nonce, session string) string {
	payload := []byte(` + "`" + `{"candidate_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","patch_id":"launch-prove","verdict":"APPROVED"}` + "`" + `)
	req := map[string]string{
		"op": "sign-verdict", "candidate_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"base_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "patch_id": "launch-prove",
		"verdict": "APPROVED", "session_id": session, "nonce": nonce,
		"payload_hex": hex.EncodeToString(payload), "mac": mac,
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return "DIAL"
	}
	defer conn.Close()
	_ = json.NewEncoder(conn).Encode(req)
	var resp struct {
		OK        bool   ` + "`json:\"ok\"`" + `
		ErrorCode string ` + "`json:\"error_code\"`" + `
	}
	_ = json.NewDecoder(conn).Decode(&resp)
	if resp.OK {
		return ""
	}
	return resp.ErrorCode
}
`
