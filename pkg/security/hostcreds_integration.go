package security

// FAC-133 integration contract (FAC-170 lands first; FAC-133 rebases after).
//
// This file defines the ONLY supported call surface for FAC-133 live harness
// proof. Do not cherry-pick FAC-133 WIP (e.g. 3bae9ab) into FAC-170. Do not
// copy FAC-133 netbroker/spawn/contain files here.
//
// ---------------------------------------------------------------------------
// Exact later FAC-133 integration point
// ---------------------------------------------------------------------------
//
// File (on FAC-133 after rebase onto main containing FAC-170):
//   pkg/herdr/live_harness_proof.go  — function proveLiveHarness
//
// Today FAC-133 gates with DiagnoseKindAuthReadiness and blocks when
// hosts_creds is empty. After FAC-170, the live path should:
//
//  1) Auth gate (unchanged shape, improved diagnosis from this package):
//
//       auth := security.DiagnoseKindAuthReadiness(kind)
//       if !auth.Brokerable {
//           pkt := security.FormatKindAuthBlocker(auth)
//           return … BLOCKED … pkt, auth.Blocker, err
//       }
//
//  2) Start session-scoped oracle BEFORE LaunchAgent / contained spawn:
//
//       store := security.NewMemorySecretStore()
//       _ = security.LoadEnvIntoStore(store) // coordinator-only env
//       sess, err := security.StartAuthorSessionNonInteractive(kind, worktree, store)
//       if err != nil { /* typed *BlockedError */ }
//       defer sess.Close()
//
//  3) Worker env / channel (least authority — no proxy bearer, no real keys):
//
//       env := sess.WorkerEnv() // dummy CLI sentinels + HERD_HOSTCREDS_SOCKET
//       fd, err := sess.OpenPreopenedFD() // preferred ExtraFiles binding
//
//  4) Sandbox network policy (FAC-133 seatbelt / limited network):
//
//       deny: security.DirectProviderHosts()  // api.x.ai, api.anthropic.com, …
//       allow: unix dial to sess.Oracle.SocketPath() OR the pre-opened FD only
//
//  5) Optional: rotate/revoke/restart via sess.Rotate / sess.Revoke / sess.Restart
//     during long sessions. Never put store secrets into agent env/argv/files.
//
// Deterministic proof (CI / herd hostcreds selftest) — not the live path:
//
//       security.ProveExactSessionHostCreds(sess, realSecret, marker)
//
// ---------------------------------------------------------------------------
// Stable production API (implemented on FAC-170)
// ---------------------------------------------------------------------------
//
//   DiagnoseKindAuthReadiness / FormatKindAuthBlocker / RequiredBrokerHostsForKind
//   CoordinatorHostCredsFromEnv / LoadEnvIntoStore / NewMemorySecretStore
//   StartAuthorSessionNonInteractive / StartHostCredsSession
//   HostCredsSession.{WorkerEnv,OpenPreopenedFD,Rotate,Revoke,Restart,Close}
//   ProveExactSessionHostCreds (tests + herd hostcreds selftest)
//   DirectProviderHosts / DummyNeverUpstream / IsDummyCredential
//   DefaultRequestRules / MatchRequestRule
//
// Compiled production caller on FAC-170 (no FAC-133 dependency):
//   herd hostcreds diagnose|session|selftest

// IntegrationAPIVersion is bumped only on breaking HostCreds API changes.
// FAC-133 should pin against this constant in comments when wiring.
const IntegrationAPIVersion = 1
