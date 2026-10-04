# Herdforge maintenance launchd job

Install `com.kampe.herdforge.worktree-lifecycle.plist` beside
`com.kampe.herdforge.maintenance-logrotate.plist` in `~/Library/LaunchAgents`.

Set `HERD_ROOT` in the launch environment to the canonical repository root.
The job runs `herd maintenance --act` every `900` seconds. It does not start
`herd pulse`, create a provider, or dispatch a lane.
