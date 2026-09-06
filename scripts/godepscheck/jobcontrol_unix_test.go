//go:build darwin || linux

package godepscheck

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// originalPipelineSnippet is the FAC-755-unfixed enumerator. The PTY fixture
// must observe this form STOP when it is a background process group of a
// controlling terminal.
const originalPipelineSnippet = `set -euo pipefail
typeset -a tracked_modules
tracked_modules=("${(@f)$(
	git ls-files -z | while IFS= read -r -d '' path; do
		case "$path" in
			go.mod|*/go.mod)
				case "/$path/" in
					*/.worktrees/*|*/.herd/*) ;;
					*) print -r -- "${path:h}" ;;
				esac
				;;
		esac
	done | LC_ALL=C sort -u
)}")
print -- "PIPELINE_OK:${#tracked_modules[@]}"
`

func TestMain(m *testing.M) {
	if os.Getenv("GODEPSCHECK_ROLE") == "pty-leader" {
		os.Exit(runPTYLeader())
	}
	os.Exit(m.Run())
}

func runPTYLeader() int {
	script := os.Getenv("GODEPSCHECK_SCRIPT")
	dir := os.Getenv("GODEPSCHECK_DIR")
	cmd := exec.Command("zsh", "-c", script)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "RESULT=STARTERR %v\n", err)
		return 1
	}
	pgid := cmd.Process.Pid
	fmt.Printf("CHILD_PGID=%d\n", pgid)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(cmd.Process.Pid, &ws, syscall.WUNTRACED|syscall.WNOHANG, nil)
		if err == nil && wpid == cmd.Process.Pid {
			fmt.Print(waitResult(ws))
			killGroup(pgid)
			return 0
		}
		if stopped, why := descendantsStopped(pgid); stopped {
			fmt.Printf("RESULT=STOPPED %s\n", why)
			killGroup(pgid)
			return 0
		}
		time.Sleep(40 * time.Millisecond)
	}
	if stopped, why := descendantsStopped(pgid); stopped {
		fmt.Printf("RESULT=STOPPED %s\n", why)
		killGroup(pgid)
		return 0
	}
	fmt.Printf("RESULT=TIMEOUT\n")
	killGroup(pgid)
	return 0
}

func waitResult(ws syscall.WaitStatus) string {
	if ws.Stopped() {
		return fmt.Sprintf("RESULT=STOPPED signal=%s\n", ws.StopSignal())
	}
	if ws.Signaled() {
		return fmt.Sprintf("RESULT=SIGNAL sig=%s\n", ws.Signal())
	}
	return fmt.Sprintf("RESULT=EXIT code=%d\n", ws.ExitStatus())
}

func killGroup(pgid int) {
	if pgid <= 1 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func descendantsStopped(rootPID int) (bool, string) {
	ps, err := exec.LookPath("ps")
	if err != nil {
		return false, ""
	}
	out, err := exec.Command(ps, "-axo", "pid=,ppid=,pgid=,state=").Output()
	if err != nil {
		return false, ""
	}
	type proc struct {
		pid, ppid, pgid int
		state           string
	}
	var all []proc
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		pgid, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		all = append(all, proc{pid: pid, ppid: ppid, pgid: pgid, state: fields[3]})
	}
	inTree := map[int]bool{rootPID: true}
	for pass := 0; pass < 8; pass++ {
		added := false
		for _, p := range all {
			if inTree[p.pid] {
				continue
			}
			if inTree[p.ppid] || p.pgid == rootPID || inTree[p.pgid] {
				inTree[p.pid] = true
				added = true
			}
		}
		if !added {
			break
		}
	}
	for _, p := range all {
		if !inTree[p.pid] && p.pgid != rootPID {
			continue
		}
		if len(p.state) > 0 && p.state[0] == 'T' {
			return true, fmt.Sprintf("pid=%d pgid=%d state=%s", p.pid, p.pgid, p.state)
		}
	}
	return false, ""
}

func TestPipelineStopsThenArrayCompletesUnderBackgroundPGID(t *testing.T) {
	requireZsh(t)
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps is required to watch child process-group state")
	}
	if probeM, probeS, err := openPTY(); err != nil {
		t.Skipf("openpty unsupported on this host: %v", err)
	} else {
		_ = probeM.Close()
		_ = probeS.Close()
	}
	dir := t.TempDir()
	initModuleRepo(t, dir)
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/root\n")
	writeFile(t, filepath.Join(dir, "mod with space/go.mod"), "module example.invalid/space\n")
	commitAll(t, dir, "modules")
	mockBin := filepath.Join(dir, "mock-bin")
	installMockTrivy(t, mockBin, filepath.Join(dir, "trivy.log"), 0)

	pipelineResult, pipelinePids := runBackgroundPGID(t, dir, originalPipelineSnippet, nil)
	if !strings.HasPrefix(pipelineResult, "STOPPED") {
		t.Fatalf("original pipeline under background PGID: got %q, want STOPPED (runtime RED)", pipelineResult)
	}

	prod := "exec " + shellSingleQuote(scriptPath(t))
	arrayResult, arrayPids := runBackgroundPGID(t, dir, prod, []string{
		"PATH=" + mockBin + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	if !strings.HasPrefix(arrayResult, "EXIT code=0") {
		t.Fatalf("repaired script under background PGID: got %q, want EXIT code=0 (runtime GREEN)", arrayResult)
	}

	deadline := time.Now().Add(2 * time.Second)
	for _, pid := range append(append([]int{}, pipelinePids...), arrayPids...) {
		if pid <= 1 {
			continue
		}
		for time.Now().Before(deadline) {
			if err := syscall.Kill(pid, 0); err != nil {
				break
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			time.Sleep(20 * time.Millisecond)
		}
		if err := syscall.Kill(pid, 0); err == nil {
			t.Fatalf("pid %d still live after cleanup", pid)
		}
	}
}

func runBackgroundPGID(t *testing.T, dir, script string, extraEnv []string) (result string, pids []int) {
	t.Helper()
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("openpty unsupported on this host: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^$")
	env := scriptEnv(dir, filepath.Join(dir, "mock-bin"))
	env = append(env,
		"GODEPSCHECK_ROLE=pty-leader",
		"GODEPSCHECK_SCRIPT="+script,
		"GODEPSCHECK_DIR="+dir,
	)
	env = append(env, extraEnv...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("Setsid/Setctty/Foreground controlling terminal not available: %v", err)
	}
	leaderPID := cmd.Process.Pid
	pids = append(pids, leaderPID)
	t.Cleanup(func() {
		_ = syscall.Kill(-leaderPID, syscall.SIGKILL)
		_ = syscall.Kill(leaderPID, syscall.SIGKILL)
	})
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}

	var collected bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(&collected, master)
		done <- copyErr
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	timeout := time.NewTimer(6 * time.Second)
	defer timeout.Stop()
	select {
	case <-waitDone:
	case <-timeout.C:
		_ = syscall.Kill(-leaderPID, syscall.SIGKILL)
		_ = syscall.Kill(leaderPID, syscall.SIGKILL)
		<-waitDone
	}
	_ = master.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	<-done

	out := collected.String()
	if pgid := parsePrefixedInt(out, "CHILD_PGID="); pgid > 1 {
		pids = append(pids, pgid)
		killGroup(pgid)
	}
	killGroup(leaderPID)

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "RESULT=") {
			return strings.TrimPrefix(line, "RESULT="), pids
		}
	}
	t.Fatalf("no RESULT line from PTY leader (output %q)", out)
	return "", pids
}

func parsePrefixedInt(out, prefix string) int {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		if err == nil {
			return n
		}
	}
	return 0
}
