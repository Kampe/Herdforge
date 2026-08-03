package security

// Dependency and integration contract for FAC-170 HostCreds.
//
// ---------------------------------------------------------------------------
// FAC-169 (hard blocker for live admission)
// ---------------------------------------------------------------------------
//
// Do NOT copy or reimplement FAC-169 separate-UID / attach / proc-mem proofs
// in this package. After FAC-169 merges to main, rebase FAC-170 and set:
//
//	security.RequireOSBoundary = /* adapter over merged FAC-169 API */
//
// Until then RequireOSBoundary defaults to BLOCKED code=fac169_required.
// No BUILD COMPLETE / review until that rebase + live proof under FAC-169.
//
// ---------------------------------------------------------------------------
// FAC-133 (after FAC-170 + FAC-169)
// ---------------------------------------------------------------------------
//
// Exact wire-up: pkg/herdr/live_harness_proof.go → proveLiveHarness
//
//  1) DiagnoseKindAuthReadinessWith(kind, auth)
//  2) HandleAuthority from HERD_HOSTCREDS_HANDLES (not raw env keys)
//  3) StartAuthorLive or StartAuthorSessionNonInteractive + WorkerEnv (MITM)
//  4) Deny DirectProviderHostsForKind(kind) outside MITM
//  5) No Get/Snapshot on CredentialAuthority
//
// Independent FAC-170 surface (no FAC-169 required for unit selftest):
//   herd hostcreds diagnose|session|selftest
// Live:
//   herd hostcreds live --kind grok   // gated on FAC-169

// IntegrationAPIVersion is bumped on breaking HostCreds API changes.
const IntegrationAPIVersion = 4
