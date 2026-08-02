package claim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// claimSubprocessHelperEnv, when set, tells TestMain to run as a helper
// process instead of the normal test binary: open the SQLite lease store
// at claimSubprocessDBPathEnv and attempt exactly one real Acquire, print
// "WON" or "CONFLICT" to stdout, and exit. This is the standard Go
// re-exec-self pattern (as used by net/http and os/exec's own tests) for
// getting genuine OS-process concurrency in a test, rather than goroutines
// sharing one process.
const (
	claimSubprocessHelperEnv = "CLAIM_SUBPROCESS_HELPER"
	claimSubprocessDBPathEnv = "CLAIM_SUBPROCESS_DB_PATH"
	claimSubprocessOwnerEnv  = "CLAIM_SUBPROCESS_OWNER"
)

func TestMain(m *testing.M) {
	if os.Getenv(claimSubprocessHelperEnv) == "1" {
		os.Exit(runClaimSubprocessHelper())
	}
	os.Exit(m.Run())
}

func runClaimSubprocessHelper() int {
	path := os.Getenv(claimSubprocessDBPathEnv)
	owner := os.Getenv(claimSubprocessOwnerEnv)

	store, err := NewSQLiteLeaseStore(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		return 2
	}
	defer store.Close()

	_, err = store.Acquire(context.Background(), testKey("FAC-SUBPROCESS"), owner, "herd-smith", "/wt", time.Now(), time.Minute)
	if err != nil {
		var conflict *ClaimConflictError
		if !errors.As(err, &conflict) && !errors.Is(err, ErrAlreadyClaimed) {
			fmt.Fprintln(os.Stderr, "unexpected acquire error:", err)
			return 3
		}
		fmt.Println("CONFLICT")
		return 0
	}
	fmt.Println("WON")
	return 0
}

// TestSQLiteLeaseStore_TrueOSProcessContention_ExactlyOneWinner spawns
// real OS subprocesses (not goroutines, not even separate *sql.DB handles
// within one process) racing Acquire against the same SQLite file, so the
// mutual-exclusion guarantee is proven the way it will actually be
// exercised in production: independent `herd` processes on the same box.
func TestSQLiteLeaseStore_TrueOSProcessContention_ExactlyOneWinner(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real OS subprocesses; skipped under -short")
	}

	path := filepath.Join(t.TempDir(), "leases.db")
	const n = 8

	type outcome struct {
		out string
		err error
	}
	results := make([]outcome, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(),
				claimSubprocessHelperEnv+"=1",
				claimSubprocessDBPathEnv+"="+path,
				fmt.Sprintf("%s=subproc-%d", claimSubprocessOwnerEnv, i),
			)
			out, err := cmd.CombinedOutput()
			results[i] = outcome{out: string(out), err: err}
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("subprocess %d failed: %v (output: %s)", i, r.err, r.out)
		}
		switch {
		case strings.Contains(r.out, "WON"):
			wins++
		case strings.Contains(r.out, "CONFLICT"):
			conflicts++
		default:
			t.Fatalf("subprocess %d produced unexpected output: %q", i, r.out)
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner across %d real OS processes, got %d wins and %d conflicts", n, wins, conflicts)
	}
	if wins+conflicts != n {
		t.Fatalf("expected %d accounted subprocess attempts, got wins=%d conflicts=%d", n, wins, conflicts)
	}
}
