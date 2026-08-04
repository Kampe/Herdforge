# FAC-188 R2 residual authority

FAC-188 can prove and reconcile CRG descendants only while the exact process
tree adapter can observe their recorded birth generation. A helper that
double-forks, calls `setsid`, and reparents to PID 1 can leave the captured
Herdr owner ancestry before inventory. Herdr pane process-info does not expose
an OS supervision handle or detached-membership token, so PPID traversal cannot
prove that residue remains launch-owned.

Required prerequisite: FAC-172/FAC-176-class spawn supervision or an equivalent
OS isolation primitive that durably returns child PID, start token, launch
generation, authenticated repository/lane/session binding, and membership that
survives double-fork/setns. Until that authority exists, FAC-188 acceptance is
explicitly limited to non-detaching CRG children; it must not claim prevention
or reaping of FAC-151 detached writers.
