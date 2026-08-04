# Confinement foundation

This package is policy planning only. It does not launch a process, install an
OS sandbox, intercept hooks or shell redirection, prevent a race after
authorization, or make a write atomic. FAC-188 remains the production
enforcement owner.

The boundary fails closed for paths that are outside the authenticated fixture,
use raw traversal, symlink or case aliases, hardlinks, or a different device.
Repo-relative paths may create nested new components beneath the nearest
canonical existing ancestor. A capability binds the canonical root and exact
sentinel device/inode identities, the complete repository/task/lease/lane/
session-generation/Herdr-tab/Herdr-pane/process/argv/policy/allowed-roots
tuple, and an issuer MAC/nonce proof. The sentinel and root identities are
re-read on every authorization.

The issuer is an explicit production-authority residual: this package only
defines its verification seam. Bind mounts that preserve the same device
number, and all OS enforcement after policy authorization, remain residuals
until an OS-specific no-follow executor seam exists.
