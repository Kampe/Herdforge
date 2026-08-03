package security

// FAC-133 integration contract (FAC-170 lands first; FAC-133 rebases after).
//
// Do not cherry-pick FAC-133 WIP into FAC-170. Do not copy FAC-133 netbroker files.
//
// Exact later FAC-133 integration point:
//   pkg/herdr/live_harness_proof.go → proveLiveHarness
//
//  1) auth := security.DiagnoseKindAuthReadinessWith(kind, auth)
//     if !auth.Brokerable { BLOCKED via FormatKindAuthBlocker }
//
//  2) Production authority (NOT raw env keys):
//       a := security.NewHandleAuthorityFromEnv() // HERD_HOSTCREDS_HANDLES only
//       // or construct HandleAuthority and InstallFromHandle per host
//
//  3) sess, err := security.StartAuthorSessionNonInteractive(kind, worktree, a)
//     defer sess.Close()
//     env := sess.WorkerEnv() // dummy sentinels + socket path only
//     fd, err := sess.OpenPreopenedFD() // preferred
//
//  4) Sandbox: deny DirectProviderHostsForKind(kind); allow only oracle FD/socket.
//
//  5) CredentialAuthority has NO Get/Snapshot. Injection only via
//     InjectAuthorization inside the oracle.
//
// Compiled production caller: herd hostcreds diagnose|session|selftest

// IntegrationAPIVersion is bumped on breaking HostCreds API changes.
const IntegrationAPIVersion = 3

// Live entry (FAC-170 production caller):
//   herd hostcreds live --kind grok
// requires HERD_HOSTCREDS_BROKER_UID ≠ worker, HERD_HOSTCREDS_BROKER_PID,
// handle-backed authority, and harness/herdr. Same-UID theater is BLOCKED.
// FAC-133 after rebase should call security.StartAuthorLive (not fake httptest).
