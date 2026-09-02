// Package security implements FAC-133 least-privilege launch policy for
// write-capable agents. Provider/board text is untrusted; control fields
// (role, cwd, review family, merge authority, lifecycle gates) come only
// from authenticated control plane + lane config. Unknown policy or
// provenance fails closed.
package security

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Provenance labels the origin of a structured task field.
type Provenance string

const (
	// ProvenanceControl is coordinator/config/lane identity (trusted).
	ProvenanceControl Provenance = "control"
	// ProvenanceProvider is board/card text (untrusted).
	ProvenanceProvider Provenance = "provider"
	// ProvenanceRepo is repository file content (untrusted).
	ProvenanceRepo Provenance = "repo"
	// ProvenanceUnknown must block execution (fail-closed).
	ProvenanceUnknown Provenance = "unknown"
)

// Role names used for least-privilege capability sets.
const (
	RoleWorker     = "worker"
	RoleReviewer   = "reviewer"
	RoleForgeSmith = "forge-smith"
	RoleBuilder    = "builder" // alias for worker write path
)

// Authority is repository write authority.
type Authority string

const (
	AuthorityRead  Authority = "read"
	AuthorityWrite Authority = "write"
)

// ExternalLinkMode controls following external URLs/downloads.
type ExternalLinkMode string

const (
	// LinkDeny blocks all external links (default fail-closed).
	LinkDeny ExternalLinkMode = "deny"
	// LinkAllowlist permits only hosts in ExternalLinkAllowlist.
	LinkAllowlist ExternalLinkMode = "allowlist"
)

// EventKind classifies observable security events.
type EventKind string

const (
	EventDenial             EventKind = "denial"
	EventInjectionIndicator EventKind = "injection_indicator"
	EventPolicyBlock        EventKind = "policy_block"
	EventProvenanceBlock    EventKind = "provenance_block"
)

// SecurityEvent is one observable denial or injection indicator.
type SecurityEvent struct {
	Kind   EventKind `json:"kind"`
	Reason string    `json:"reason"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// EventSink records security events. Implementations must surface I/O errors
// (fail closed for durable audit). Nil sinks are rejected by LaunchAgent.
type EventSink interface {
	Record(ev SecurityEvent) error
}

// MemorySink is a thread-safe in-memory EventSink (tests + process-local).
type MemorySink struct {
	mu     sync.Mutex
	Events []SecurityEvent
}

// Record appends an event.
func (m *MemorySink) Record(ev SecurityEvent) error {
	if m == nil {
		return fmt.Errorf("%w: nil memory sink", ErrUnknownPolicy)
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	m.mu.Lock()
	m.Events = append(m.Events, ev)
	m.mu.Unlock()
	return nil
}

// Snapshot returns a copy of recorded events.
func (m *MemorySink) Snapshot() []SecurityEvent {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SecurityEvent, len(m.Events))
	copy(out, m.Events)
	return out
}

// StructuredTask is the sanitized view of a provider task: untrusted text is
// quoted with provenance; control fields are never taken from provider text.
type StructuredTask struct {
	Ref             string
	Title           string
	TitleProvenance Provenance
	Description     string
	DescProvenance  Provenance
	// Control-only (must be ProvenanceControl; provider text cannot set these):
	Role             string
	RoleProvenance   Provenance
	CWD              string
	CWDProvenance    Provenance
	ReviewFamily     string
	ReviewProvenance Provenance
	MergeAuthority   bool
	MergeProvenance  Provenance
	LifecycleGate    string
	LifeProvenance   Provenance
}

// Validate fails closed when any control field has non-control provenance or
// any field is unknown provenance.
func (t *StructuredTask) Validate() error {
	if t == nil {
		return fmt.Errorf("%w: nil structured task", ErrUnknownPolicy)
	}
	if t.Ref == "" {
		return fmt.Errorf("%w: missing task ref", ErrUnknownPolicy)
	}
	for _, p := range []Provenance{t.TitleProvenance, t.DescProvenance, t.RoleProvenance, t.CWDProvenance, t.ReviewProvenance, t.MergeProvenance, t.LifeProvenance} {
		if p == ProvenanceUnknown || p == "" {
			return fmt.Errorf("%w: field provenance unknown", ErrUnknownProvenance)
		}
	}
	// Control fields must not be provider/repo derived.
	if t.RoleProvenance != ProvenanceControl {
		return fmt.Errorf("%w: role provenance %q (provider cannot set role)", ErrProviderAuthority, t.RoleProvenance)
	}
	if t.CWDProvenance != ProvenanceControl {
		return fmt.Errorf("%w: cwd provenance %q", ErrProviderAuthority, t.CWDProvenance)
	}
	if t.ReviewProvenance != ProvenanceControl {
		return fmt.Errorf("%w: review family provenance %q", ErrProviderAuthority, t.ReviewProvenance)
	}
	if t.MergeProvenance != ProvenanceControl {
		return fmt.Errorf("%w: merge authority provenance %q", ErrProviderAuthority, t.MergeProvenance)
	}
	if t.LifeProvenance != ProvenanceControl {
		return fmt.Errorf("%w: lifecycle gate provenance %q", ErrProviderAuthority, t.LifeProvenance)
	}
	if t.TitleProvenance == ProvenanceControl || t.DescProvenance == ProvenanceControl {
		return fmt.Errorf("%w: provider body elevated to control provenance", ErrProviderAuthority)
	}
	return nil
}

// LaunchPolicy is the fail-closed least-privilege contract for one agent launch.
// Validate must succeed before AuthorizeLaunch. Missing policy is a hard error.
type LaunchPolicy struct {
	// ControlSecret must be non-empty for any write-capable launch (FAC-133).
	ControlSecret string

	// FilesystemRoot is the only allowed cwd/root (task worktree). Absolute.
	FilesystemRoot string
	// SharedCheckout is the denied shared repository root. Absolute.
	SharedCheckout string
	// RepoAllowlist is the set of repository identities this agent may touch.
	// Empty allowlist fails closed.
	RepoAllowlist []string
	// RepoIdentity is the active repository (must be in allowlist).
	RepoIdentity string

	// Role and Authority drive capability defaults.
	Role      string
	Authority Authority

	// AllowedTools is the explicit tool allowlist. Empty fails closed for launch.
	AllowedTools []string
	// DeniedTools always blocks even if also listed in AllowedTools.
	DeniedTools []string

	// Network: offline | limited | online. Empty fails closed.
	Network string
	// NetworkAllowHosts used when Network is limited (broker CONNECT destinations).
	// Must not include generic localhost (localhost canary denial).
	NetworkAllowHosts []string
	// BrokerEndpoint is host:port of the durable proxy (exact OS allow for limited).
	BrokerEndpoint string
	// TestCommand is the repo verification contract (e.g. "go test ./...").
	// Used to derive process-exec allowlist entries for workers/reviewers.
	TestCommand string

	// SecretDeny is the list of env var names that must NOT appear in agent env.
	// Defaults include integration credentials when not set via DefaultSecretDeny.
	SecretDeny []string

	// ExternalLinks mode + host allowlist.
	ExternalLinks         ExternalLinkMode
	ExternalLinkAllowlist []string

	// IntegrationCredentials: true only for control-plane roles that need
	// board/merge credentials. Builders and reviewers must be false.
	IntegrationCredentials bool

	// PackageAllowlist, when non-empty and ExclusivePackages, restricts
	// AuthorizePath to those package prefixes under FilesystemRoot.
	PackageAllowlist  []string
	ExclusivePackages bool

	// Events records denials and injection indicators.
	Events EventSink
}

// Sentinel errors (fail-closed, exit-propagating at CLI).
var (
	ErrUnknownPolicy        = errors.New("security: unknown or incomplete launch policy (fail-closed)")
	ErrUnknownProvenance    = errors.New("security: unknown content provenance (fail-closed)")
	ErrProviderAuthority    = errors.New("security: provider text cannot alter control authority")
	ErrSharedRoot           = errors.New("security: shared checkout / sibling path denied")
	ErrRepoNotAllowlisted   = errors.New("security: repository not in allowlist")
	ErrPathDenied           = errors.New("security: filesystem path denied")
	ErrToolDenied           = errors.New("security: tool not allowlisted")
	ErrNetworkDenied        = errors.New("security: network access denied")
	ErrSecretPresent        = errors.New("security: forbidden secret in agent environment")
	ErrExternalLinkDenied   = errors.New("security: external link denied by policy")
	ErrIntegrationCreds     = errors.New("security: integration credentials denied for role")
	ErrMissingControlSecret = errors.New("security: HERD_CONTROL_SECRET required (fail-closed)")
	ErrReviewerWrite        = errors.New("security: reviewer is read-only")
	ErrBuilderIntegration   = errors.New("security: builder cannot access integration credentials")
)

// DefaultSecretDeny is the env deny-list for builder/reviewer launches.
func DefaultSecretDeny() []string {
	return []string{
		"KANEO_API_KEY",
		"KANEO_TOKEN",
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"LINEAR_API_KEY",
		"JIRA_API_TOKEN",
		"AZURE_DEVOPS_PAT",
		"AWS_SECRET_ACCESS_KEY",
		"OPENAI_API_KEY",
		"ANTHROPIC_API_KEY",
		// Control secret itself must not leak into the agent process env
		// as a general secret; coordinator holds it out-of-band.
		"HERD_CONTROL_SECRET",
		"HERD_MERGE_TOKEN",
		"HERD_APPROVE_TOKEN",
	}
}

// DefaultToolsForRole returns the fail-closed tool allowlist for a role.
func DefaultToolsForRole(role string) []string {
	switch strings.ToLower(role) {
	case RoleReviewer:
		return []string{"git-read", "read-file", "grep", "herd-verify-read"}
	case RoleWorker, RoleBuilder, RoleForgeSmith:
		return []string{"git-write", "read-file", "write-file", "grep", "shell-exec", "herd-verify"}
	default:
		return nil
	}
}

// Validate fails closed when policy is incomplete or role/capability inconsistent.
func (p *LaunchPolicy) Validate() error {
	if p == nil {
		return ErrUnknownPolicy
	}
	if strings.TrimSpace(p.ControlSecret) == "" {
		return ErrMissingControlSecret
	}
	if p.FilesystemRoot == "" || p.SharedCheckout == "" {
		return fmt.Errorf("%w: filesystem root and shared checkout required", ErrUnknownPolicy)
	}
	if len(p.RepoAllowlist) == 0 || p.RepoIdentity == "" {
		return fmt.Errorf("%w: repo allowlist and identity required", ErrUnknownPolicy)
	}
	if !containsFold(p.RepoAllowlist, p.RepoIdentity) {
		return ErrRepoNotAllowlisted
	}
	if p.Role == "" || p.Authority == "" {
		return fmt.Errorf("%w: role and authority required", ErrUnknownPolicy)
	}
	if len(p.AllowedTools) == 0 {
		return fmt.Errorf("%w: empty tool allowlist", ErrUnknownPolicy)
	}
	if p.Network == "" {
		return fmt.Errorf("%w: network policy required", ErrUnknownPolicy)
	}
	if p.ExternalLinks == "" {
		return fmt.Errorf("%w: external link policy required", ErrUnknownPolicy)
	}
	if len(p.SecretDeny) == 0 {
		return fmt.Errorf("%w: secret deny list required", ErrUnknownPolicy)
	}

	// Role-specific hard rules.
	role := strings.ToLower(p.Role)
	switch role {
	case RoleReviewer:
		if p.Authority != AuthorityRead {
			return ErrReviewerWrite
		}
		if p.IntegrationCredentials {
			return ErrIntegrationCreds
		}
		if containsFold(p.AllowedTools, "git-write") || containsFold(p.AllowedTools, "board-write") {
			return ErrReviewerWrite
		}
	case RoleWorker, RoleBuilder, RoleForgeSmith:
		if p.IntegrationCredentials {
			return ErrBuilderIntegration
		}
	}

	// Roots must differ (shared root never allowed as cwd).
	absFS, err1 := filepath.Abs(p.FilesystemRoot)
	absShared, err2 := filepath.Abs(p.SharedCheckout)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("%w: resolve roots", ErrUnknownPolicy)
	}
	if absFS == absShared {
		return ErrSharedRoot
	}
	return nil
}

// LaunchRequest is the concrete launch intent checked against policy.
type LaunchRequest struct {
	// CWD must equal policy.FilesystemRoot.
	CWD string
	// Role requested — must match policy.Role (provider cannot override).
	Role string
	// Tools the agent surface intends to enable.
	Tools []string
	// Env is the environment map that would be passed to the agent.
	Env map[string]string
	// Paths the launch preamble intends to open (optional precheck).
	Paths []string
	// ExternalURLs from packet/provider text that would be followed.
	ExternalURLs []string
	// MergeRequested / BoardWriteRequested are elevated authorities.
	MergeRequested      bool
	BoardWriteRequested bool
	// ProviderText is scanned for injection indicators (events only; never elevates).
	ProviderText string
	// Structured is the provenance-tagged task view.
	Structured *StructuredTask
}

// LaunchGrant is the authorized, reduced surface after policy checks.
type LaunchGrant struct {
	CWD            string
	Role           string
	Authority      Authority
	AllowedTools   []string
	Network        string
	SanitizedEnv   map[string]string
	PackageRoots   []string
	EventsRecorded int
}

// AuthorizeLaunch is the production gate for write-capable (and reviewer)
// agent starts. Fail-closed on any violation; records security events.
func (p *LaunchPolicy) AuthorizeLaunch(req LaunchRequest) (*LaunchGrant, error) {
	if err := p.Validate(); err != nil {
		p.record(EventPolicyBlock, err.Error(), "")
		return nil, err
	}
	if req.Structured != nil {
		if err := req.Structured.Validate(); err != nil {
			p.record(EventProvenanceBlock, err.Error(), "")
			return nil, err
		}
	} else {
		p.record(EventProvenanceBlock, "missing structured task", "")
		return nil, fmt.Errorf("%w: structured task required", ErrUnknownProvenance)
	}

	// Scan provider text for injection indicators (observable; does not elevate).
	n := p.ScanProviderText(req.ProviderText)
	_ = n

	// Role cannot be altered by request vs policy.
	if req.Role != "" && !strings.EqualFold(req.Role, p.Role) {
		p.record(EventDenial, "role mismatch", req.Role)
		return nil, fmt.Errorf("%w: requested role %q != policy role %q", ErrProviderAuthority, req.Role, p.Role)
	}

	// CWD must be the filesystem root (worktree), never shared checkout.
	if err := p.AuthorizeCWD(req.CWD); err != nil {
		p.record(EventDenial, err.Error(), req.CWD)
		return nil, err
	}

	// Elevated authorities.
	if req.MergeRequested {
		p.record(EventDenial, "merge authority requested", p.Role)
		return nil, fmt.Errorf("%w: merge", ErrProviderAuthority)
	}
	if req.BoardWriteRequested && strings.EqualFold(p.Role, RoleReviewer) {
		p.record(EventDenial, "reviewer board-write", "")
		return nil, ErrReviewerWrite
	}
	if req.BoardWriteRequested && !p.IntegrationCredentials {
		p.record(EventDenial, "board write without integration credentials", p.Role)
		return nil, ErrIntegrationCreds
	}

	for _, tool := range req.Tools {
		if err := p.AuthorizeTool(tool); err != nil {
			p.record(EventDenial, err.Error(), tool)
			return nil, err
		}
	}
	for _, path := range req.Paths {
		if err := p.AuthorizePath(path); err != nil {
			p.record(EventDenial, err.Error(), path)
			return nil, err
		}
	}
	// External URLs under LinkDeny become inert (record + drop) rather than
	// failing the whole launch — prevents trivial board DoS via a URL in a card.
	// Actual fetches still require AuthorizeExternalURL at the fetch boundary.
	if len(req.ExternalURLs) > 0 {
		_, _ = InertExternalURLs(p, req.ExternalURLs)
	}
	if err := p.AuthorizeEnv(req.Env); err != nil {
		p.record(EventDenial, err.Error(), "")
		return nil, err
	}

	sanitized := sanitizeEnv(req.Env, p.SecretDeny)
	tools := append([]string(nil), p.AllowedTools...)
	pkgs := append([]string(nil), p.PackageAllowlist...)
	return &LaunchGrant{
		CWD:            p.FilesystemRoot,
		Role:           p.Role,
		Authority:      p.Authority,
		AllowedTools:   tools,
		Network:        p.Network,
		SanitizedEnv:   sanitized,
		PackageRoots:   pkgs,
		EventsRecorded: n,
	}, nil
}

// AuthorizeCWD ensures cwd is the policy filesystem root and not the shared checkout.
func (p *LaunchPolicy) AuthorizeCWD(cwd string) error {
	if cwd == "" {
		return fmt.Errorf("%w: empty cwd", ErrPathDenied)
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPathDenied, err)
	}
	absFS, _ := filepath.Abs(p.FilesystemRoot)
	absShared, _ := filepath.Abs(p.SharedCheckout)
	if absCWD == absShared {
		return ErrSharedRoot
	}
	if absCWD != absFS {
		// Also deny sibling paths (escape from worktree).
		if !isUnder(absCWD, absFS) {
			return fmt.Errorf("%w: cwd %q outside filesystem root %q", ErrPathDenied, cwd, p.FilesystemRoot)
		}
	}
	return nil
}

// AuthorizePath denies sibling repos, shared checkout, and out-of-allowlist packages.
func (p *LaunchPolicy) AuthorizePath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty path", ErrPathDenied)
	}
	// Reject path traversal and absolute escapes toward shared root.
	clean := filepath.Clean(path)
	if strings.Contains(clean, "..") {
		return fmt.Errorf("%w: path traversal", ErrPathDenied)
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		// Relative path: resolve against filesystem root.
		abs = filepath.Join(p.FilesystemRoot, clean)
		abs, _ = filepath.Abs(abs)
	}
	absFS, _ := filepath.Abs(p.FilesystemRoot)
	absShared, _ := filepath.Abs(p.SharedCheckout)
	if abs == absShared || isUnder(abs, absShared) && !isUnder(abs, absFS) {
		// Under shared checkout but not under worktree → sibling/shared deny.
		if !isUnder(abs, absFS) {
			return ErrSharedRoot
		}
	}
	if !isUnder(abs, absFS) {
		return fmt.Errorf("%w: %q not under worktree root", ErrPathDenied, path)
	}
	if p.ExclusivePackages && len(p.PackageAllowlist) > 0 {
		rel, err := filepath.Rel(absFS, abs)
		if err != nil {
			return ErrPathDenied
		}
		ok := false
		for _, pkg := range p.PackageAllowlist {
			pkg = strings.Trim(pkg, "/")
			if rel == pkg || strings.HasPrefix(rel, pkg+string(filepath.Separator)) {
				ok = true
				break
			}
		}
		// Allow the root itself and policy/packet files at worktree root.
		if rel == "." || !strings.Contains(rel, string(filepath.Separator)) {
			ok = true
		}
		if !ok {
			return fmt.Errorf("%w: package allowlist exclusive: %q", ErrPathDenied, rel)
		}
	}
	return nil
}

// AuthorizeTool checks tool allow/deny lists.
func (p *LaunchPolicy) AuthorizeTool(tool string) error {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return fmt.Errorf("%w: empty tool", ErrToolDenied)
	}
	if containsFold(p.DeniedTools, tool) {
		return fmt.Errorf("%w: %q denied", ErrToolDenied, tool)
	}
	if !containsFold(p.AllowedTools, tool) {
		return fmt.Errorf("%w: %q not allowlisted", ErrToolDenied, tool)
	}
	// Reviewer hard deny of write tools.
	if strings.EqualFold(p.Role, RoleReviewer) {
		switch strings.ToLower(tool) {
		case "git-write", "write-file", "shell-exec", "board-write":
			return ErrReviewerWrite
		}
	}
	return nil
}

// AuthorizeEnv fails if any denied secret name is present with a non-empty value.
func (p *LaunchPolicy) AuthorizeEnv(env map[string]string) error {
	if env == nil {
		return nil
	}
	for _, name := range p.SecretDeny {
		if v, ok := env[name]; ok && strings.TrimSpace(v) != "" {
			return fmt.Errorf("%w: %s", ErrSecretPresent, name)
		}
	}
	// Builders/reviewers: deny integration credential patterns by prefix.
	if !p.IntegrationCredentials {
		for k, v := range env {
			if strings.TrimSpace(v) == "" {
				continue
			}
			uk := strings.ToUpper(k)
			if strings.Contains(uk, "API_KEY") || strings.Contains(uk, "API_TOKEN") ||
				strings.HasSuffix(uk, "_PAT") || strings.Contains(uk, "MERGE_TOKEN") {
				return fmt.Errorf("%w: %s", ErrSecretPresent, k)
			}
		}
	}
	return nil
}

// AuthorizeExternalURL enforces external link policy.
func (p *LaunchPolicy) AuthorizeExternalURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	switch p.ExternalLinks {
	case LinkDeny:
		return fmt.Errorf("%w: %q", ErrExternalLinkDenied, raw)
	case LinkAllowlist:
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return fmt.Errorf("%w: malformed %q", ErrExternalLinkDenied, raw)
		}
		host := strings.ToLower(u.Host)
		for _, allowed := range p.ExternalLinkAllowlist {
			if host == strings.ToLower(allowed) || strings.HasSuffix(host, "."+strings.ToLower(allowed)) {
				return nil
			}
		}
		return fmt.Errorf("%w: host %q", ErrExternalLinkDenied, host)
	default:
		return fmt.Errorf("%w: unknown external link mode", ErrUnknownPolicy)
	}
}

// AuthorizeNetwork checks whether outbound network is permitted.
func (p *LaunchPolicy) AuthorizeNetwork() error {
	switch strings.ToLower(p.Network) {
	case "offline":
		return fmt.Errorf("%w: offline policy", ErrNetworkDenied)
	case "limited", "online":
		return nil
	default:
		return fmt.Errorf("%w: network %q", ErrUnknownPolicy, p.Network)
	}
}

// ScanProviderText records injection indicators; never elevates trust.
// Returns the number of indicators found.
func (p *LaunchPolicy) ScanProviderText(text string) int {
	if text == "" {
		return 0
	}
	lower := strings.ToLower(text)
	indicators := []struct {
		sub    string
		reason string
	}{
		{"ignore previous instructions", "indirect_prompt_injection"},
		{"ignore all previous", "indirect_prompt_injection"},
		{"disregard prior", "indirect_prompt_injection"},
		{"you are now", "role_override_attempt"},
		{"act as system", "role_override_attempt"},
		{"sudo ", "shell_instruction"},
		{"rm -rf", "shell_instruction"},
		{"curl http", "shell_instruction"},
		{"wget ", "shell_instruction"},
		{"/etc/passwd", "secret_request"},
		{".ssh/", "secret_request"},
		{"api_key", "secret_request"},
		{"private key", "secret_request"},
		{"merge to main", "merge_authority_request"},
		{"herd approve", "merge_authority_request"},
		{"grant reviewer", "reviewer_authority_request"},
		{"http://", "external_link"},
		{"https://", "external_link"},
	}
	n := 0
	for _, ind := range indicators {
		if strings.Contains(lower, ind.sub) {
			p.record(EventInjectionIndicator, ind.reason, ind.sub)
			n++
		}
	}
	return n
}

// StructureTask builds a StructuredTask from provider data + control bindings.
// Provider fields are always ProvenanceProvider; control fields always Control.
func StructureTask(ref, title, description, role, cwd, reviewFamily, lifecycleGate string, merge bool) *StructuredTask {
	return &StructuredTask{
		Ref:              ref,
		Title:            title,
		TitleProvenance:  ProvenanceProvider,
		Description:      description,
		DescProvenance:   ProvenanceProvider,
		Role:             role,
		RoleProvenance:   ProvenanceControl,
		CWD:              cwd,
		CWDProvenance:    ProvenanceControl,
		ReviewFamily:     reviewFamily,
		ReviewProvenance: ProvenanceControl,
		MergeAuthority:   merge,
		MergeProvenance:  ProvenanceControl,
		LifecycleGate:    lifecycleGate,
		LifeProvenance:   ProvenanceControl,
	}
}

// PolicyForLane builds a fail-closed LaunchPolicy for a role/lane/worktree.
// controlSecret empty → Validate fails. Reviewers are read-only; builders
// never receive integration credentials.
func PolicyForLane(role, worktreePath, sharedCheckout, repoIdentity string, repoAllowlist []string, controlSecret string, packageAllowlist []string) (*LaunchPolicy, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return nil, fmt.Errorf("%w: empty role (unknown policy must block)", ErrUnknownPolicy)
	}
	switch role {
	case RoleWorker, RoleReviewer, RoleBuilder, RoleForgeSmith:
	default:
		return nil, fmt.Errorf("%w: unknown role %q", ErrUnknownPolicy, role)
	}
	auth := AuthorityWrite
	integration := false
	// Workers and reviewers both use limited network: OS seatbelt is loopback-only
	// and HostAllowBroker permits only harness/model hosts. Full offline would
	// block the AI CLI model transport required for reviewers.
	network := "limited"
	links := LinkDeny
	if role == RoleReviewer {
		auth = AuthorityRead
	}
	tools := DefaultToolsForRole(role)
	if tools == nil {
		return nil, fmt.Errorf("%w: unknown role %q", ErrUnknownPolicy, role)
	}
	allow := append([]string(nil), repoAllowlist...)
	if len(allow) == 0 && repoIdentity != "" {
		allow = []string{repoIdentity}
	}
	// limited: broker-enforced allow-hosts (seatbelt remains loopback-only).
	var netHosts []string
	if network == "limited" {
		netHosts = DefaultHarnessAllowHosts()
	}
	p := &LaunchPolicy{
		ControlSecret:          controlSecret,
		FilesystemRoot:         worktreePath,
		SharedCheckout:         sharedCheckout,
		RepoAllowlist:          allow,
		RepoIdentity:           repoIdentity,
		Role:                   role,
		Authority:              auth,
		AllowedTools:           tools,
		DeniedTools:            []string{},
		Network:                network,
		NetworkAllowHosts:      netHosts,
		TestCommand:            "go test ./...",
		SecretDeny:             DefaultSecretDeny(),
		ExternalLinks:          links,
		IntegrationCredentials: integration,
		PackageAllowlist:       nil,
		ExclusivePackages:      false,
		Events:                 &MemorySink{},
	}
	if len(packageAllowlist) > 0 {
		norm, nerr := NormalizePackageAllowlist(packageAllowlist)
		if nerr != nil {
			return nil, nerr
		}
		p.PackageAllowlist = norm
		p.ExclusivePackages = true
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *LaunchPolicy) record(kind EventKind, reason, detail string) error {
	if p == nil || p.Events == nil {
		return fmt.Errorf("%w: event sink required", ErrUnknownPolicy)
	}
	return p.Events.Record(SecurityEvent{Kind: kind, Reason: reason, Detail: detail, At: time.Now().UTC()})
}

// RecordFatal records an event and returns a non-nil error if persistence fails.
// Detail is redacted to strip host absolute paths before persistence.
func (p *LaunchPolicy) RecordFatal(kind EventKind, reason, detail string) error {
	shared := ""
	if p != nil {
		shared = p.SharedCheckout
	}
	detail = RedactAbsPaths(detail, shared)
	reason = RedactAbsPaths(reason, shared)
	if err := p.record(kind, reason, detail); err != nil {
		return fmt.Errorf("security event persistence failed: %w", err)
	}
	return nil
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

func isUnder(child, parent string) bool {
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)
	if child == parent {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(parent, sep) {
		parent += sep
	}
	return strings.HasPrefix(child, parent)
}

func sanitizeEnv(env map[string]string, deny []string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		denied := false
		for _, d := range deny {
			if k == d {
				denied = true
				break
			}
		}
		if denied {
			continue
		}
		uk := strings.ToUpper(k)
		if strings.Contains(uk, "API_KEY") || strings.Contains(uk, "API_TOKEN") || strings.HasSuffix(uk, "_PAT") {
			continue
		}
		out[k] = v
	}
	return out
}
