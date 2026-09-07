# CLI persistence and user services

This PR implements the CLI portion of #1014: retain a computer's enrollment and
run it through the current OS user's service manager. Configuration and service
lifecycle are separate operations.

| Command | Writes | Failure / retry |
| --- | --- | --- |
| `enroll --server … --key …` or `enroll --config …` | One private enrollment file | Existing enrollment changes require `--replace`; an atomic file replacement leaves a complete old or new document. No service commands. |
| `run` | Nothing | Reads saved enrollment. Explicit flags/environment or `--config` provide a temporary connection. |
| `service install` | Fresh program generation, installation record, native service definition | Never writes enrollment or starts a process. An interrupted registration leaves a `prepared` record; rerun install. |
| `service start` | Native process state | Requires completed installation and readable enrollment; never installs or recovers. Already-running service is a no-op. |
| `service restart` | Native process state | Validates first, then stops and starts. No automatic rollback. |
| `service stop` | Native process state | Does not read enrollment or installation records, and never starts anything. |
| `service uninstall [--purge]` | Native registration and installation record; optionally managed programs/logs | Does not read enrollment or installation records, and never starts anything. Enrollment is retained. |
| `service status` | Nothing | Reports the OS service state. It does not claim server connectivity. |

There is one saved enrollment at `~/.memoh/runtime.json`. Service programs,
registration files, the installation record and the operation lock are owned by
`~/.memoh/runtime/`. Custom writable storage locations are intentionally outside
this PR. `--config` is only an input, not a managed storage override.

The native definition always reads the saved enrollment. Explicit reenrollment
changes what the next process will read; a running process keeps its current
connection until the user restarts it. Reenrollment is not a service install.

Installation prepares immutable program files and validates the platform
representation before stopping any existing process. It then publishes a
`prepared` record, registers the service without starting it, and publishes an
`installed` record. A failed operation reports failure and leaves these files
for inspection/retry. No journal, historical credential copies, or automatic
cross-system rollback is needed. Old enrollment is never part of the install
transaction. A failed registration can always be followed by stop, uninstall or
another install; only start requires an installed record.

All mutations use the same operation lock. A CLI killed while holding the lock
requires checking the recorded owner PID and removing the abandoned lock before
retrying. We deliberately do not reclaim locks based only on elapsed time.

Platform adapters expose register/start/stop/status/uninstall. Registration has
no process-start side effect. Restart is composed once at the command layer.
Linux registers the fixed unit path with `systemctl --user enable <absolute path>`;
launchd uses the user's LaunchAgents directory; Windows tasks include the user's
SID. These identifiers do not depend on the enrollment or installation record.

Trust checks and private atomic writes remain necessary. The supported namespace
is limited to the owned storage above. File creation occurs inside an already
private staging directory to avoid inherited-ACL pre-open races. Input files are
read-only, and unsafe directory chains/executable ACLs are rejected.
