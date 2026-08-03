package ledger

import (
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFreshPostgresMigration applies Phase 1 to a brand-new PostgreSQL cluster.
// It is skipped only where PostgreSQL's local test binaries are unavailable.
func TestFreshPostgresMigration(t *testing.T) {
	for _, binary := range []string{"initdb", "pg_ctl", "psql"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("fresh PostgreSQL migration coverage requires %s: %v", binary, err)
		}
	}

	dataDir := filepath.Join(t.TempDir(), "postgres")
	runPostgres(t, "initdb", "-D", dataDir, "--no-locale", "--encoding=UTF8", "--auth=trust")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve postgres port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release postgres port: %v", err)
	}

	runPostgres(t, "pg_ctl", "-D", dataDir, "-l", filepath.Join(dataDir, "server.log"), "-o", fmt.Sprintf("-h 127.0.0.1 -p %d", port), "-w", "start")
	t.Cleanup(func() { _ = exec.Command("pg_ctl", "-D", dataDir, "-m", "immediate", "stop").Run() })

	psql := func(sql string) error {
		cmd := exec.Command("psql", "-X", "-v", "ON_ERROR_STOP=1", "-h", "127.0.0.1", "-p", fmt.Sprint(port), "-d", "postgres")
		cmd.Stdin = strings.NewReader(sql)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("psql: %w\n%s", err, output)
		}
		return nil
	}

	if err := psql(MigrationSQL()); err != nil {
		t.Fatalf("apply fresh migration: %v", err)
	}
	if err := psql(MigrationSQL()); err != nil {
		t.Fatalf("migration must be idempotent: %v", err)
	}

	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	digest := "sha256:" + strings.Repeat("c", 64)
	if err := psql(fmt.Sprintf(`
INSERT INTO herdforge.actors (id, kind, display_name) VALUES ('00000000-0000-0000-0000-000000000001', 'operator', 'local operator');
INSERT INTO herdforge.principals (id, actor_id, kind, label) VALUES ('00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000001', 'local_operator', 'operator');
INSERT INTO herdforge.runs (id, repository, base_sha, owner_principal_id, status) VALUES ('00000000-0000-0000-0000-000000000003', 'Herdforge', '%s', '00000000-0000-0000-0000-000000000002', 'running');
INSERT INTO herdforge.phases (id, run_id, ordinal, kind, status) VALUES ('00000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-000000000003', 0, 'implementation', 'active');
INSERT INTO herdforge.candidates (id, run_id, phase_id, git_sha, base_sha, evidence_digest) VALUES ('00000000-0000-0000-0000-000000000005', '00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000004', '%s', '%s', '%s');
INSERT INTO herdforge.receipts (id, candidate_id, kind, evidence_digest) VALUES ('00000000-0000-0000-0000-000000000006', '00000000-0000-0000-0000-000000000005', 'verification', '%s');
INSERT INTO herdforge.reviews (id, candidate_id, reviewer_principal_id, outcome, receipt_id) VALUES ('00000000-0000-0000-0000-000000000007', '00000000-0000-0000-0000-000000000005', '00000000-0000-0000-0000-000000000002', 'pass', '00000000-0000-0000-0000-000000000006');
INSERT INTO herdforge.approvals (id, candidate_id, approver_principal_id, decision, receipt_id) VALUES ('00000000-0000-0000-0000-000000000008', '00000000-0000-0000-0000-000000000005', '00000000-0000-0000-0000-000000000002', 'approved', '00000000-0000-0000-0000-000000000006');
INSERT INTO herdforge.spend_entries (id, run_id, actor_id, principal_id, amount_usd, token_count) VALUES ('00000000-0000-0000-0000-000000000009', '00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', 1.25, 12);
INSERT INTO herdforge.owned_worktrees (id, run_id, candidate_id, worktree_path, branch, base_sha, owner_principal_id) VALUES ('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000005', './.herd/worktrees/SPE-562', 'herd/spe-562', '%s', '00000000-0000-0000-0000-000000000002');
INSERT INTO herdforge.lifecycle_events (id, run_id, sequence, event_type, actor_id, principal_id, phase_id, candidate_id, evidence_digest, idempotency_key) VALUES ('00000000-0000-0000-0000-000000000011', '00000000-0000-0000-0000-000000000003', 1, 'candidate.reported', '00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-000000000005', '%s', 'candidate.reported:1');`, shaA, shaB, shaA, digest, digest, shaA, digest)); err != nil {
		t.Fatalf("insert every Phase 1 contract: %v", err)
	}

	if err := psql(fmt.Sprintf("UPDATE herdforge.candidates SET git_sha = '%s'", shaA)); err == nil {
		t.Fatal("candidate git SHA mutation must be rejected")
	}
	if err := psql("UPDATE herdforge.candidates SET evidence_digest = 'sha256:deadbeef'"); err == nil {
		t.Fatal("candidate evidence digest mutation must be rejected")
	}
	if err := psql("UPDATE herdforge.lifecycle_events SET event_type = 'forged'"); err == nil {
		t.Fatal("lifecycle event update must be rejected")
	}
	if err := psql("DELETE FROM herdforge.lifecycle_events"); err == nil {
		t.Fatal("lifecycle event delete must be rejected")
	}
}

func runPostgres(t *testing.T, binary string, args ...string) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", binary, strings.Join(args, " "), err, output)
	}
}
