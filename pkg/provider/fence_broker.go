package provider

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"golang.org/x/sys/unix"

	_ "modernc.org/sqlite"
)

const (
	envFenceBrokerURL       = "HERD_FENCE_BROKER_URL"
	envFenceBrokerToken     = "HERD_FENCE_BROKER_TOKEN"
	envFenceBrokerMintToken = "HERD_FENCE_BROKER_MINT_TOKEN"
	envFenceBrokerListen    = "HERD_FENCE_BROKER_LISTEN"
	brokerLockLeaf          = "fence-broker.lock"
	brokerSockLeaf          = "fence-broker.sock"
	brokerAuthHeader        = "X-Herd-Broker-Token"
	leasesDBLeaf            = "leases.db"
)

// FenceBroker is the production-enforcing sidecar for FAC-147.
// Process exclusivity is the claim-dir flock (one live broker). Per-task
// serialization uses in-process mutexes — not fences.db.locks — so a client
// CAS holding the shared store lock while calling the broker cannot deadlock.
type FenceBroker struct {
	mu         sync.Mutex
	taskMu     map[string]*sync.Mutex
	store      FenceStore
	leases     *claim.SQLiteLeaseStore // authoritative lease validation
	upstream   *KaneoProvider
	token      string // worker mutate credential (cannot mint)
	mintToken  string // coordinator/claim mint credential
	claimDir   string
	instanceID string
	lockFD     int
	srv        *http.Server
	ln         net.Listener
	network    string
	sockPath   string
	grantDB    *sql.DB // durable capability grant state machine
	started    time.Time
	// test-only fault injection (same package tests)
	testFailUpstream int // remaining upstream failures before success
	testFailMark     int // remaining MarkApplied failures after upstream ok
}

// FenceBrokerConfig configures the sidecar.
type FenceBrokerConfig struct {
	ClaimDir        string
	ListenAddr      string // "unix" (default), 127.0.0.1:port — never non-loopback
	Token           string // worker token
	MintToken       string // mint token (required; must differ from Token)
	UpstreamURL     string
	UpstreamProject string
	UpstreamCLI     bool
}

// StartFenceBroker acquires exclusive claim-dir lock and serves.
// Non-copyable authority is the exclusive flock on the claim volume — not a
// copyable absolute claim_path row. Same logical path on a shared volume cannot
// host two live brokers. Cross-host without shared flock is fail-closed/undefined.
func StartFenceBroker(cfg FenceBrokerConfig) (*FenceBroker, error) {
	if strings.TrimSpace(cfg.ClaimDir) == "" {
		return nil, fmt.Errorf("fence-broker: ClaimDir required")
	}
	if strings.TrimSpace(cfg.Token) == "" || len(cfg.Token) < 16 {
		return nil, fmt.Errorf("fence-broker: Token required (min 16)")
	}
	if strings.TrimSpace(cfg.MintToken) == "" || len(cfg.MintToken) < 16 {
		return nil, fmt.Errorf("fence-broker: MintToken required (min 16; workers must not hold this)")
	}
	if cfg.MintToken == cfg.Token {
		return nil, fmt.Errorf("fence-broker: MintToken must differ from worker Token")
	}
	abs, err := filepath.Abs(cfg.ClaimDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}

	lockPath := filepath.Join(abs, brokerLockLeaf)
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fence-broker: open lock: %w", err)
	}
	cleanup := func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("fence-broker: exclusive flock failed (another live broker on this claim volume): %w", err)
	}

	// Production binary always requires shared seal (ALLOW_LOCAL cannot bypass).
	// Hermetic tests may skip only when not requiring shared and no VOLUME_ID.
	if !testing.Testing() {
		if err := ValidateSharedMarker(abs); err != nil {
			cleanup()
			return nil, fmt.Errorf("fence-broker: shared seal required in production (ALLOW_LOCAL ignored): %w", err)
		}
	} else if os.Getenv("HERD_FENCE_REQUIRE_SHARED") == "1" || strings.TrimSpace(os.Getenv("HERD_FENCE_VOLUME_ID")) != "" {
		if err := ValidateSharedMarker(abs); err != nil {
			cleanup()
			return nil, fmt.Errorf("fence-broker: shared seal: %w", err)
		}
	}

	store, err := NewSQLiteFenceStore(filepath.Join(abs, fencesDBLeaf))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("fence-broker: open fences.db: %w", err)
	}
	leases, err := claim.NewSQLiteLeaseStore(filepath.Join(abs, leasesDBLeaf))
	if err != nil {
		_ = store.Close()
		cleanup()
		return nil, fmt.Errorf("fence-broker: open leases.db: %w", err)
	}
	grantDB, err := openGrantDB(filepath.Join(abs, "capability-grants.db"))
	if err != nil {
		_ = leases.Close()
		_ = store.Close()
		cleanup()
		return nil, err
	}

	network, listenAddr, sockPath, err := resolveBrokerListen(abs, cfg.ListenAddr)
	if err != nil {
		_ = grantDB.Close()
		_ = leases.Close()
		_ = store.Close()
		cleanup()
		return nil, err
	}
	if network == "unix" {
		// Only unlink if it is a socket under claim dir (never foreign path).
		if err := safeUnlinkUnixSocket(sockPath, abs); err != nil {
			_ = grantDB.Close()
			_ = leases.Close()
			_ = store.Close()
			cleanup()
			return nil, err
		}
	}

	up := NewKaneoProvider(cfg.UpstreamURL, cfg.UpstreamProject, cfg.UpstreamCLI)
	up.RequireCASMeta = false
	up.AtomicFenceServer = false
	up.Receiver = nil
	up.FenceBroker = nil

	var idb [16]byte
	if _, err := rand.Read(idb[:]); err != nil {
		_ = grantDB.Close()
		_ = leases.Close()
		_ = store.Close()
		cleanup()
		return nil, fmt.Errorf("fence-broker: instance id: %w", err)
	}

	b := &FenceBroker{
		store: store, leases: leases, upstream: up,
		token: cfg.Token, mintToken: cfg.MintToken,
		claimDir: abs, instanceID: hex.EncodeToString(idb[:]),
		lockFD: fd, network: network, sockPath: sockPath,
		grantDB: grantDB, started: time.Now().UTC(),
		taskMu: make(map[string]*sync.Mutex),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", b.handleHealth)
	mux.HandleFunc("/v1/status", b.handleStatus)
	mux.HandleFunc("/v1/capabilities", b.handleCapabilities)
	mux.HandleFunc("/v1/ops/", b.handleOp)
	mux.HandleFunc("/v1/tasks/", b.handleTask)

	ln, err := net.Listen(network, listenAddr)
	if err != nil {
		_ = grantDB.Close()
		_ = leases.Close()
		_ = store.Close()
		cleanup()
		return nil, fmt.Errorf("fence-broker: listen: %w", err)
	}
	if network == "unix" {
		if err := os.Chmod(listenAddr, 0o600); err != nil {
			_ = ln.Close()
			_ = grantDB.Close()
			_ = leases.Close()
			_ = store.Close()
			cleanup()
			return nil, fmt.Errorf("fence-broker: chmod socket: %w", err)
		}
	}
	b.ln = ln
	b.srv = &http.Server{Handler: b.routeAuth(mux), ReadHeaderTimeout: 10 * time.Second}
	// Coordinator mint credential lives only as claim-dir file mode 0600 — never env.
	if err := b.writeMintCredential(); err != nil {
		_ = ln.Close()
		_ = grantDB.Close()
		_ = leases.Close()
		_ = store.Close()
		cleanup()
		return nil, fmt.Errorf("fence-broker: mint credential: %w", err)
	}
	go func() { _ = b.srv.Serve(ln) }()
	return b, nil
}

func openGrantDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Durable grant state machine: pending → applied | ambiguous.
	// Never burn a grant before effect success (provider-fail must leave pending).
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS capability_grants (
		nonce TEXT PRIMARY KEY NOT NULL,
		op_id TEXT NOT NULL,
		repo TEXT NOT NULL,
		provider TEXT NOT NULL,
		project TEXT NOT NULL,
		task_ref TEXT NOT NULL,
		board_task_id TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		generation INTEGER NOT NULL,
		status TEXT NOT NULL,
		state TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Durable "remote HTTP 2xx observed" log — recovery without re-mutate when
	// crash lands between remote commit and grant upstream_committed.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS upstream_ok (
		op_id TEXT PRIMARY KEY NOT NULL,
		board_task_id TEXT NOT NULL,
		status TEXT NOT NULL,
		fence INTEGER NOT NULL,
		recorded_at TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_grants_op ON capability_grants(op_id)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func resolveBrokerListen(claimDir, listen string) (network, addr, sockPath string, err error) {
	listen = strings.TrimSpace(listen)
	if listen == "" || listen == "unix" {
		sock := filepath.Join(claimDir, brokerSockLeaf)
		return "unix", sock, sock, nil
	}
	if strings.HasPrefix(listen, "unix:") {
		sock := strings.TrimPrefix(listen, "unix:")
		if !isUnderDir(sock, claimDir) {
			return "", "", "", fmt.Errorf("fence-broker: unix socket must be under claim dir %q", claimDir)
		}
		return "unix", sock, sock, nil
	}
	host, port, e := net.SplitHostPort(listen)
	if e != nil {
		return "", "", "", fmt.Errorf("fence-broker: ListenAddr: %w", e)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", "", "", fmt.Errorf("fence-broker: refusing non-loopback ListenAddr %q (use unix socket under claim dir or 127.0.0.1)", listen)
	}
	return "tcp", net.JoinHostPort(host, port), "", nil
}

func isUnderDir(path, dir string) bool {
	ap, err1 := filepath.Abs(path)
	ad, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(ad, ap)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func safeUnlinkUnixSocket(sockPath, claimDir string) error {
	if !isUnderDir(sockPath, claimDir) {
		return fmt.Errorf("fence-broker: refuse unlink outside claim dir")
	}
	// O_NOFOLLOW-style: lstat and refuse if symlink.
	fi, err := os.Lstat(sockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("fence-broker: refuse unlink symlink socket")
	}
	return os.Remove(sockPath)
}

func (b *FenceBroker) Addr() string {
	if b == nil || b.ln == nil {
		return ""
	}
	if b.network == "unix" {
		return "unix://" + b.ln.Addr().String()
	}
	return b.ln.Addr().String()
}

func (b *FenceBroker) ClientBaseURL() string {
	if b == nil || b.ln == nil {
		return ""
	}
	if b.network == "unix" {
		return "http://unix"
	}
	return "http://" + b.ln.Addr().String()
}

func (b *FenceBroker) UnixSocket() string {
	if b == nil || b.network != "unix" {
		return ""
	}
	return b.ln.Addr().String()
}

func (b *FenceBroker) InstanceID() string {
	if b == nil {
		return ""
	}
	return b.instanceID
}

// Close shuts down and releases flock; aggregates errors (fail-closed report).
// Secrets (token, mintToken) are intentionally not exported — no Token()/MintToken().
func (b *FenceBroker) Close() error {
	if b == nil {
		return nil
	}
	var errs []string
	if b.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := b.srv.Shutdown(ctx); err != nil {
			errs = append(errs, "shutdown: "+err.Error())
		}
		cancel()
	}
	if b.network == "unix" && b.sockPath != "" {
		if err := safeUnlinkUnixSocket(b.sockPath, b.claimDir); err != nil && !os.IsNotExist(err) {
			errs = append(errs, "unlink socket: "+err.Error())
		}
	}
	if b.grantDB != nil {
		if err := b.grantDB.Close(); err != nil {
			errs = append(errs, "grant db: "+err.Error())
		}
	}
	if b.leases != nil {
		if err := b.leases.Close(); err != nil {
			errs = append(errs, "leases: "+err.Error())
		}
	}
	if b.store != nil {
		if err := b.store.Close(); err != nil {
			errs = append(errs, "store: "+err.Error())
		}
	}
	if b.lockFD >= 0 {
		if err := unix.Flock(b.lockFD, unix.LOCK_UN); err != nil {
			errs = append(errs, "flock unlock: "+err.Error())
		}
		if err := unix.Close(b.lockFD); err != nil {
			errs = append(errs, "close lock fd: "+err.Error())
		}
		b.lockFD = -1
	}
	if len(errs) > 0 {
		return fmt.Errorf("fence-broker close: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (b *FenceBroker) routeAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		// Mint endpoint requires mint token exclusively (workers must not hold it).
		if r.URL.Path == "/v1/capabilities" {
			mt := r.Header.Get(mintAuthHeader)
			if mt == "" || mt != b.mintToken {
				http.Error(w, `{"error":"mint token required (workers cannot mint)"}`, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		tok := r.Header.Get(brokerAuthHeader)
		if tok == "" {
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if tok != b.token {
			http.Error(w, `{"error":"unauthorized broker token"}`, http.StatusUnauthorized)
			return
		}
		// Worker token must not equal mint token for defense in depth.
		if tok == b.mintToken {
			http.Error(w, `{"error":"mint token cannot be used as worker credential"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (b *FenceBroker) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "role": "fence-broker", "claim_dir": b.claimDir,
		"instance_id": b.instanceID, "started_at": b.started.Format(time.RFC3339),
		"network": b.network,
	})
}

func (b *FenceBroker) handleStatus(w http.ResponseWriter, r *http.Request) {
	b.handleHealth(w, r)
}

// POST /v1/capabilities — mint only (mint token + durable full-LeaseKey validation).
// action=status (default) requires status; action=comment requires comment body.
func (b *FenceBroker) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method"}`, 405)
		return
	}
	var body struct {
		BoardTaskID string `json:"board_task_id"`
		TaskID      string `json:"task_id"`
		TaskRef     string `json:"task_ref"`
		Repo        string `json:"repo"`
		Provider    string `json:"provider"`
		Project     string `json:"project"`
		Generation  int64  `json:"generation"`
		OwnerID     string `json:"owner_id"`
		OpID        string `json:"op_id"`
		Action      string `json:"action"` // status | comment
		Status      string `json:"status"`
		Comment     string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"bad json"}`, 400)
		return
	}
	boardID := body.BoardTaskID
	if boardID == "" {
		boardID = body.TaskID
	}
	if body.TaskRef == "" {
		body.TaskRef = boardID
	}
	if boardID == "" || body.TaskRef == "" {
		http.Error(w, `{"error":"task_ref and board_task_id required (or single task_id for both)"}`, 400)
		return
	}
	if body.Repo == "" || body.Provider == "" || body.Project == "" {
		http.Error(w, `{"error":"full LeaseKey required: repo, provider, project, task_ref"}`, 400)
		return
	}
	if body.OwnerID == "" || body.OpID == "" || body.Generation <= 0 {
		http.Error(w, `{"error":"owner_id, generation, op_id required"}`, 400)
		return
	}
	action := body.Action
	if action == "" {
		action = capActionStatus
	}
	if action == capActionStatus && body.Status == "" {
		http.Error(w, `{"error":"status required for status action"}`, 400)
		return
	}
	if action == capActionComment && body.Comment == "" {
		http.Error(w, `{"error":"comment required for comment action"}`, 400)
		return
	}
	if action != capActionStatus && action != capActionComment {
		http.Error(w, `{"error":"action must be status or comment"}`, 400)
		return
	}
	key := claim.LeaseKey{Repo: body.Repo, Provider: body.Provider, Project: body.Project, TaskRef: body.TaskRef}
	now := time.Now()
	live, err := b.lookupLiveLease(r.Context(), now, key, body.OwnerID, body.Generation)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 500)
		return
	}
	if live == nil {
		http.Error(w, `{"error":"no active lease matching full LeaseKey+owner+generation"}`, http.StatusForbidden)
		return
	}
	expUnix := live.ExpiresAt.UTC().Unix()
	var cap string
	if action == capActionComment {
		cap, err = MintCommentCapability(b.mintToken, b.instanceID, key, boardID, body.OwnerID, body.OpID, body.Comment, body.Generation, expUnix)
	} else {
		cap, err = MintMutationCapability(b.mintToken, b.instanceID, key, boardID, body.OwnerID, body.OpID, body.Status, body.Generation, expUnix)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 400)
		return
	}
	if err := b.registerGrantPending(cap); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 500)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"capability": cap, "instance_id": b.instanceID,
		"expires_at": live.ExpiresAt.UTC().Format(time.RFC3339),
		"state":      grantStatePending,
	})
}

func (b *FenceBroker) lookupLiveLease(ctx context.Context, now time.Time, key claim.LeaseKey, ownerID string, generation int64) (*claim.Lease, error) {
	leases, err := b.leases.ActiveClaims(ctx, now)
	if err != nil {
		return nil, err
	}
	for _, l := range leases {
		if l == nil {
			continue
		}
		if l.LeaseKey == key && l.OwnerID == ownerID && l.Generation == generation && !l.Expired(now) {
			return l, nil
		}
	}
	return nil, nil
}

func (b *FenceBroker) registerGrantPending(capJSON string) error {
	var c MutationCapability
	if err := json.Unmarshal([]byte(capJSON), &c); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := b.grantDB.Exec(`INSERT INTO capability_grants (
		nonce, op_id, repo, provider, project, task_ref, board_task_id,
		owner_id, generation, status, state, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Nonce, c.OpID, c.Repo, c.Provider, c.Project, c.TaskRef, c.BoardTaskID,
		c.OwnerID, c.Generation, c.Status, grantStatePending, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("capability nonce already registered")
		}
		return err
	}
	return nil
}

type grantRow struct {
	Nonce       string
	OpID        string
	Repo        string
	Provider    string
	Project     string
	TaskRef     string
	BoardTaskID string
	OwnerID     string
	Generation  int64
	Status      string
	State       string
}

func (b *FenceBroker) loadGrant(nonce string) (*grantRow, error) {
	var g grantRow
	err := b.grantDB.QueryRow(`SELECT nonce, op_id, repo, provider, project, task_ref, board_task_id,
		owner_id, generation, status, state FROM capability_grants WHERE nonce = ?`, nonce).
		Scan(&g.Nonce, &g.OpID, &g.Repo, &g.Provider, &g.Project, &g.TaskRef, &g.BoardTaskID,
			&g.OwnerID, &g.Generation, &g.Status, &g.State)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (b *FenceBroker) setGrantState(nonce, state string) error {
	res, err := b.grantDB.Exec(`UPDATE capability_grants SET state = ?, updated_at = ? WHERE nonce = ?`,
		state, time.Now().UTC().Format(time.RFC3339Nano), nonce)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("grant state update missed nonce")
	}
	return nil
}

func (b *FenceBroker) recordUpstreamOK(opID, boardTaskID, status string, fence int64) error {
	_, err := b.grantDB.Exec(`INSERT OR REPLACE INTO upstream_ok (op_id, board_task_id, status, fence, recorded_at) VALUES (?, ?, ?, ?, ?)`,
		opID, boardTaskID, NormalizeStatus(status), fence, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (b *FenceBroker) hasUpstreamOK(opID string) (bool, error) {
	var n int
	err := b.grantDB.QueryRow(`SELECT COUNT(1) FROM upstream_ok WHERE op_id = ?`, opID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// writeMintCredential writes coordinator-only mint secret under claim dir (0600).
// Not placed in process environment. Workers must not receive this path.
func (b *FenceBroker) writeMintCredential() error {
	if b == nil || b.claimDir == "" || b.mintToken == "" {
		return fmt.Errorf("fence-broker: cannot write mint credential")
	}
	path := filepath.Join(b.claimDir, mintCredLeaf)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.mintToken+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

const mintCredLeaf = "fence-mint.cred"

func (b *FenceBroker) handleOp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method"}`, 405)
		return
	}
	opID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/ops/"), "/")
	rec, err := b.store.LookupApplied(r.Context(), opID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if rec == nil {
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"applied": false})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"applied": !rec.Ambiguous, "ambiguous": rec.Ambiguous,
		"op_id": rec.OpID, "task_id": rec.TaskID, "fence_token": rec.FenceToken,
		"expected_status": rec.ExpectedStatus, "revision": rec.Revision,
	})
}

func (b *FenceBroker) handleTask(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		http.Error(w, `{"error":"missing task"}`, 400)
		return
	}
	taskID := parts[0]
	if len(parts) >= 2 && parts[1] == "fence" && r.Method == http.MethodGet {
		high, err := b.store.Highest(r.Context(), taskID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": taskID, "highest": high})
		return
	}
	if len(parts) >= 2 && parts[1] == "status" && r.Method == http.MethodPut {
		b.handleStatusMutate(w, r, taskID)
		return
	}
	if len(parts) >= 2 && parts[1] == "comment" && r.Method == http.MethodPost {
		b.handleCommentMutate(w, r, taskID)
		return
	}
	http.Error(w, `{"error":"unhandled"}`, 404)
}

func (b *FenceBroker) handleStatusMutate(w http.ResponseWriter, r *http.Request, boardTaskID string) {
	opID := r.Header.Get("X-Herd-Op")
	fenceHdr := r.Header.Get("X-Herd-Fence")
	capRaw := r.Header.Get(mutationCapabilityHeader)
	if opID == "" || fenceHdr == "" {
		http.Error(w, `{"error":"missing fence/op"}`, 400)
		return
	}
	fence, err := strconv.ParseInt(fenceHdr, 10, 64)
	if err != nil || fence <= 0 {
		http.Error(w, `{"error":"bad fence"}`, 400)
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil || body.Status == "" {
		http.Error(w, `{"error":"status required"}`, 400)
		return
	}
	status := NormalizeStatus(body.Status)

	// Capability MAC uses mint secret (workers cannot forge).
	cap, err := VerifyMutationCapability(b.mintToken, b.instanceID, capRaw, boardTaskID, opID, status, fence)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"capability: %s"}`, err.Error()), http.StatusForbidden)
		return
	}

	// In-process exclusive (claim-dir flock already ensures one broker process).
	err = b.withTaskLock(boardTaskID, func() error {
		ctx := r.Context()
		grant, gerr := b.loadGrant(cap.Nonce)
		if gerr != nil {
			return gerr
		}
		if grant == nil {
			return fmt.Errorf("capability grant unknown")
		}
		if grant.OpID != opID || grant.BoardTaskID != boardTaskID || grant.Generation != fence ||
			NormalizeStatus(grant.Status) != status || grant.OwnerID != cap.OwnerID ||
			grant.Repo != cap.Repo || grant.Provider != cap.Provider || grant.Project != cap.Project ||
			grant.TaskRef != cap.TaskRef {
			return fmt.Errorf("grant binding mismatch")
		}

		prev, err := b.store.LookupApplied(ctx, opID)
		if err != nil {
			return err
		}
		// Durable op receipt is the only success signal (never status equality).
		if prev != nil && !prev.Ambiguous &&
			prev.TaskID == boardTaskID && prev.FenceToken == fence &&
			NormalizeStatus(prev.ExpectedStatus) == status {
			if err := b.setGrantState(cap.Nonce, grantStateApplied); err != nil {
				return err
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"deduped":true}`))
			return errWritten
		}

		// Re-validate live lease with full LeaseKey at mutate time.
		now := time.Now()
		live, lerr := b.lookupLiveLease(ctx, now, cap.LeaseKey(), cap.OwnerID, fence)
		if lerr != nil {
			return lerr
		}
		if live == nil {
			return errNoLiveLease
		}

		// Durable upstream_ok log recovers crash after remote 2xx without re-mutate.
		upOK, err := b.hasUpstreamOK(opID)
		if err != nil {
			return err
		}

		// --- Reconcile without remote re-mutate (stock Kaneo has no op dedupe) ---
		switch grant.State {
		case grantStateApplied:
			return errAmbiguousFailClosed
		case grantStateInFlight:
			if upOK {
				// Remote commit was durably logged; seal receipt only.
				if err := b.setGrantState(cap.Nonce, grantStateUpstream); err != nil {
					return fmt.Errorf("%w: %v", errLocalFail, err)
				}
				return b.sealLocalReceipt(ctx, w, cap.Nonce, opID, boardTaskID, status, fence)
			}
			// Unknown whether remote committed (timeout/loss) — never re-mutate.
			return errAmbiguousFailClosed
		case grantStateUpstream, grantStateAmbiguous:
			return b.sealLocalReceipt(ctx, w, cap.Nonce, opID, boardTaskID, status, fence)
		case grantStatePending:
			if upOK {
				// Prior success log without grant advance — seal only.
				if err := b.setGrantState(cap.Nonce, grantStateUpstream); err != nil {
					return fmt.Errorf("%w: %v", errLocalFail, err)
				}
				return b.sealLocalReceipt(ctx, w, cap.Nonce, opID, boardTaskID, status, fence)
			}
			// proceed to remote once
		default:
			return fmt.Errorf("unknown grant state %q", grant.State)
		}

		high, err := b.store.Highest(ctx, boardTaskID)
		if err != nil {
			return err
		}
		if fence < high {
			return errStaleFence
		}
		if fence > high {
			if _, err := b.store.Advance(ctx, boardTaskID, fence); err != nil {
				return err
			}
		}

		// Pre-send inject: fail before in_flight so pure provider-fail can retry.
		if b.testFailUpstream > 0 {
			b.testFailUpstream--
			return fmt.Errorf("%w: injected upstream failure", errProviderFail)
		}

		// Durable in_flight BEFORE remote — any error after this is fail-closed
		// (HTTP timeout may mean remote already committed; never reset to pending).
		if err := b.setGrantState(cap.Nonce, grantStateInFlight); err != nil {
			return err
		}

		// Upstream is stock Kaneo under the broker. Use the full-schema PUT
		// path without toggling shared AtomicFenceServer (data race under
		// concurrent multi-task mutates; also a correctness flip if another
		// request restores prevAtomic mid-flight).
		upErr := b.upstream.mutateStatusFullSchemaPUT(context.WithoutCancel(ctx), boardTaskID, status)
		if err := upErr; err != nil {
			// Do NOT reset pending: arbitrary provider errors are not pre-commit proof.
			return fmt.Errorf("%w: after in_flight (no remutate): %v", errAmbiguousFailClosed, err)
		}

		// Record remote success durably before grant transition (crash recovery).
		if err := b.recordUpstreamOK(opID, boardTaskID, status, fence); err != nil {
			return fmt.Errorf("%w: record upstream_ok: %v", errAmbiguousFailClosed, err)
		}
		if err := b.setGrantState(cap.Nonce, grantStateUpstream); err != nil {
			// upstream_ok is durable — seal path can still recover via hasUpstreamOK.
			return fmt.Errorf("%w: record upstream_committed: %v", errLocalFail, err)
		}

		return b.sealLocalReceipt(ctx, w, cap.Nonce, opID, boardTaskID, status, fence)
	})
	if err == errWritten {
		return
	}
	if err == errStaleFence {
		http.Error(w, `{"error":"stale fence"}`, 409)
		return
	}
	if err == errOpMeta {
		http.Error(w, `{"error":"op metadata mismatch"}`, 409)
		return
	}
	if err == errNoLiveLease {
		http.Error(w, `{"error":"no live lease for full LeaseKey+owner+generation (expired or reclaimed)"}`, http.StatusForbidden)
		return
	}
	if err != nil && errors.Is(err, errAmbiguousFailClosed) {
		// Not retryable as re-mutate; client must not replay status blindly.
		http.Error(w, fmt.Sprintf(`{"error":%q,"retryable":false,"ambiguous":true}`, err.Error()), 409)
		return
	}
	if err != nil && errors.Is(err, errLocalFail) {
		// Local receipt only — retryable without remote re-mutate.
		http.Error(w, fmt.Sprintf(`{"error":%q,"retryable":true,"local_receipt":true}`, err.Error()), 503)
		return
	}
	if err != nil && errors.Is(err, errProviderFail) {
		http.Error(w, fmt.Sprintf(`{"error":%q,"retryable":true}`, err.Error()), 503)
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 500)
	}
}

// handleCommentMutate enforces the same grant state machine as status for comments.
func (b *FenceBroker) handleCommentMutate(w http.ResponseWriter, r *http.Request, boardTaskID string) {
	opID := r.Header.Get("X-Herd-Op")
	fenceHdr := r.Header.Get("X-Herd-Fence")
	capRaw := r.Header.Get(mutationCapabilityHeader)
	if opID == "" || fenceHdr == "" {
		http.Error(w, `{"error":"missing fence/op"}`, 400)
		return
	}
	fence, err := strconv.ParseInt(fenceHdr, 10, 64)
	if err != nil || fence <= 0 {
		http.Error(w, `{"error":"bad fence"}`, 400)
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil || body.Body == "" {
		http.Error(w, `{"error":"body required"}`, 400)
		return
	}
	cap, err := VerifyCommentCapability(b.mintToken, b.instanceID, capRaw, boardTaskID, opID, body.Body, fence)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"capability: %s"}`, err.Error()), http.StatusForbidden)
		return
	}
	err = b.withTaskLock(boardTaskID, func() error {
		ctx := r.Context()
		grant, gerr := b.loadGrant(cap.Nonce)
		if gerr != nil {
			return gerr
		}
		if grant == nil {
			return fmt.Errorf("capability grant unknown")
		}
		if grant.OpID != opID || grant.BoardTaskID != boardTaskID || grant.Generation != fence ||
			grant.OwnerID != cap.OwnerID || grant.TaskRef != cap.TaskRef {
			return fmt.Errorf("grant binding mismatch")
		}
		prev, err := b.store.LookupApplied(ctx, opID)
		if err != nil {
			return err
		}
		if prev != nil && !prev.Ambiguous && prev.TaskID == boardTaskID && prev.FenceToken == fence &&
			prev.ExpectedComment == body.Body {
			_ = b.setGrantState(cap.Nonce, grantStateApplied)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"deduped":true}`))
			return errWritten
		}
		now := time.Now()
		live, lerr := b.lookupLiveLease(ctx, now, cap.LeaseKey(), cap.OwnerID, fence)
		if lerr != nil {
			return lerr
		}
		if live == nil {
			return errNoLiveLease
		}
		upOK, err := b.hasUpstreamOK(opID)
		if err != nil {
			return err
		}
		switch grant.State {
		case grantStateInFlight:
			if upOK {
				_ = b.setGrantState(cap.Nonce, grantStateUpstream)
				return b.sealLocalCommentReceipt(ctx, w, cap.Nonce, opID, boardTaskID, body.Body, fence)
			}
			return errAmbiguousFailClosed
		case grantStateUpstream, grantStateAmbiguous:
			return b.sealLocalCommentReceipt(ctx, w, cap.Nonce, opID, boardTaskID, body.Body, fence)
		case grantStatePending:
			if upOK {
				_ = b.setGrantState(cap.Nonce, grantStateUpstream)
				return b.sealLocalCommentReceipt(ctx, w, cap.Nonce, opID, boardTaskID, body.Body, fence)
			}
		case grantStateApplied:
			return errAmbiguousFailClosed
		default:
			return fmt.Errorf("unknown grant state %q", grant.State)
		}
		if err := b.setGrantState(cap.Nonce, grantStateInFlight); err != nil {
			return err
		}
		// Upstream comment (unfenced stock path on broker's upstream provider).
		if err := b.upstream.addCommentRaw(ctx, boardTaskID, body.Body); err != nil {
			return fmt.Errorf("%w: after in_flight (no remutate): %v", errAmbiguousFailClosed, err)
		}
		if err := b.recordUpstreamOK(opID, boardTaskID, "comment", fence); err != nil {
			return fmt.Errorf("%w: record upstream_ok: %v", errAmbiguousFailClosed, err)
		}
		if err := b.setGrantState(cap.Nonce, grantStateUpstream); err != nil {
			return fmt.Errorf("%w: %v", errLocalFail, err)
		}
		return b.sealLocalCommentReceipt(ctx, w, cap.Nonce, opID, boardTaskID, body.Body, fence)
	})
	if err == errWritten {
		return
	}
	if err == errNoLiveLease {
		http.Error(w, `{"error":"no live lease"}`, http.StatusForbidden)
		return
	}
	if err != nil && errors.Is(err, errAmbiguousFailClosed) {
		http.Error(w, fmt.Sprintf(`{"error":%q,"retryable":false,"ambiguous":true}`, err.Error()), 409)
		return
	}
	if err != nil && errors.Is(err, errLocalFail) {
		http.Error(w, fmt.Sprintf(`{"error":%q,"retryable":true,"local_receipt":true}`, err.Error()), 503)
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), 500)
	}
}

func (b *FenceBroker) sealLocalCommentReceipt(ctx context.Context, w http.ResponseWriter, nonce, opID, boardTaskID, comment string, fence int64) error {
	if err := b.store.MarkApplied(ctx, OpReceipt{
		OpID: opID, TaskID: boardTaskID, FenceToken: fence,
		ExpectedComment: comment, Revision: "broker:" + opID,
	}); err != nil {
		_ = b.store.MarkAmbiguous(ctx, OpReceipt{OpID: opID, TaskID: boardTaskID, FenceToken: fence, ExpectedComment: comment})
		_ = b.setGrantState(nonce, grantStateAmbiguous)
		return fmt.Errorf("%w: mark applied: %v", errLocalFail, err)
	}
	if err := b.setGrantState(nonce, grantStateApplied); err != nil {
		return fmt.Errorf("%w: %v", errLocalFail, err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "op_id": opID, "task_id": boardTaskID})
	return errWritten
}

// sealLocalReceipt writes durable op evidence only (never calls stock Kaneo).
func (b *FenceBroker) sealLocalReceipt(ctx context.Context, w http.ResponseWriter, nonce, opID, boardTaskID, status string, fence int64) error {
	if b.testFailMark > 0 {
		b.testFailMark--
		if err := b.store.MarkAmbiguous(ctx, OpReceipt{
			OpID: opID, TaskID: boardTaskID, FenceToken: fence, ExpectedStatus: status,
		}); err != nil {
			return fmt.Errorf("%w: mark ambiguous: %v", errLocalFail, err)
		}
		if err := b.setGrantState(nonce, grantStateAmbiguous); err != nil {
			return fmt.Errorf("%w: grant ambiguous: %v", errLocalFail, err)
		}
		return fmt.Errorf("%w: injected mark applied failure", errLocalFail)
	}
	if err := b.store.MarkApplied(ctx, OpReceipt{
		OpID: opID, TaskID: boardTaskID, FenceToken: fence,
		ExpectedStatus: status, Revision: "broker:" + opID,
	}); err != nil {
		if merr := b.store.MarkAmbiguous(ctx, OpReceipt{
			OpID: opID, TaskID: boardTaskID, FenceToken: fence, ExpectedStatus: status,
		}); merr != nil {
			return fmt.Errorf("%w: mark applied: %v; mark ambiguous: %v", errLocalFail, err, merr)
		}
		if gerr := b.setGrantState(nonce, grantStateAmbiguous); gerr != nil {
			return fmt.Errorf("%w: mark applied: %v; grant state: %v", errLocalFail, err, gerr)
		}
		return fmt.Errorf("%w: mark applied: %v", errLocalFail, err)
	}
	if err := b.setGrantState(nonce, grantStateApplied); err != nil {
		return fmt.Errorf("%w: grant applied after receipt: %v", errLocalFail, err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "op_id": opID, "task_id": boardTaskID,
		"fence_token": fence, "status": status, "grant_state": grantStateApplied,
	})
	return errWritten
}

func (b *FenceBroker) withTaskLock(taskID string, fn func() error) error {
	if fn == nil {
		return fmt.Errorf("nil fn")
	}
	b.mu.Lock()
	if b.taskMu == nil {
		b.taskMu = make(map[string]*sync.Mutex)
	}
	m, ok := b.taskMu[taskID]
	if !ok {
		m = &sync.Mutex{}
		b.taskMu[taskID] = m
	}
	b.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	return fn()
}

// SeedTestLease inserts a live lease for tests (same claim-dir leases.db).
func (b *FenceBroker) SeedTestLease(ctx context.Context, key claim.LeaseKey, ownerID string, generation int64, ttl time.Duration) (*claim.Lease, error) {
	if b == nil || b.leases == nil {
		return nil, fmt.Errorf("nil broker leases")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	now := time.Now()
	lease, err := b.leases.Acquire(ctx, key, ownerID, "worker", "", now, ttl)
	if err != nil {
		return nil, err
	}
	if generation > 0 && lease.Generation != generation {
		for lease.Generation < generation {
			_, _, _ = b.leases.Release(ctx, key, ownerID, lease.Generation, now)
			lease, err = b.leases.Acquire(ctx, key, ownerID, "worker", "", now, ttl)
			if err != nil {
				return nil, err
			}
		}
	}
	return lease, nil
}

type brokerSentinel string

func (e brokerSentinel) Error() string { return string(e) }

var (
	errWritten             = brokerSentinel("written")
	errStaleFence          = brokerSentinel("stale")
	errOpMeta              = brokerSentinel("opmeta")
	errNoLiveLease         = brokerSentinel("no-live-lease")
	errProviderFail        = brokerSentinel("provider-fail")
	errLocalFail           = brokerSentinel("local-fail")
	errAmbiguousFailClosed = brokerSentinel("ambiguous-fail-closed-no-remutate")
)
