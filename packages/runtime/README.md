# Memoh Remote Runtime CLI

The CLI runs a Remote Runtime in the foreground or installs a background service
for the current OS user: launchd on macOS, systemd user services on Linux, and
Task Scheduler on Windows. Windows tasks are scoped to the account SID and
require that user to be logged in. On Linux the service lives and dies with the
user's systemd instance; run `loginctl enable-linger` to keep it running after
logout or to start it at boot without a login.

```sh
# Save the connection for future foreground and service runs.
memoh-runtime enroll --server https://memoh.example --key "$MEMOH_RUNTIME_KEY"
memoh-runtime run

# Install the service, then explicitly start it.
memoh-runtime service install
memoh-runtime service start
memoh-runtime service status --json
memoh-runtime service stop

# Change the saved connection and apply it to the service.
memoh-runtime enroll --server https://memoh.example --key "$NEW_RUNTIME_KEY" --replace
memoh-runtime service restart

memoh-runtime service uninstall
```

`enroll` saves the single enrollment at `~/.memoh/runtime.json`. Any change,
including key rotation, requires `--replace`. Saving does not test the server
connection or affect an already-running process. Run or restart to use the saved
connection. `--replace` can also repair a malformed saved enrollment.

`run --server ... --key ...` makes a temporary connection without saving.
Foreground invocation without the `run` subcommand remains supported. Explicit
`--config <file>` reads only that file, ignores connection environment variables,
and cannot be combined with connection flags. `MEMOH_RUNTIME_CONFIG` selects an
input file; it does not change where `enroll` saves or what the service reads.
Managed paths are fixed; `MEMOH_RUNTIME_HOME` is not supported.

`service install` copies the program into a fresh directory, validates the
platform definition, stops the previous service, and registers its replacement.
It does not read credentials and does not start the service in the current
session. Registration enables automatic startup on a future login/user-manager
startup. Reinstallation also leaves the service stopped. `service start` and
`service restart` validate the installed program and saved enrollment before
starting; restart validates before stopping the old process.

`service stop` ends the current process only. The registration stays in place on
every platform, so the service starts again at the next login or user-manager
startup; use `service uninstall` to remove it for good.

If registration fails, installation retains a `prepared` record and its program
files. Rerun `service install` to complete installation. There is no automatic
rollback or recovery that starts another process. `stop` and `uninstall` do not
parse credentials or installation records, so malformed files do not block them.
Operations share a lock under `~/.memoh/runtime/operation.lock`. If the CLI was
killed, check `owner.json`, confirm that process has exited, then remove the lock
directory before retrying. Never remove a lock owned by a live process.

`status` reports the OS process state, not server connectivity. Inspect
`~/.memoh/runtime/logs/` on macOS and Windows or `journalctl --user -u
memoh-runtime` on Linux for failures; Task Scheduler captures no output, so the
Windows service passes `run --log` to keep `logs/runtime.log` itself. A start
check cannot guarantee future process health.

Uninstall removes the native service and installation record, preserving saved
enrollment. `uninstall --purge` also removes the program directory, service
definitions, and logs. A successful install removes the generations it replaced;
a failed one leaves its files for inspection until the next successful install.
Installations from earlier revisions of this unmerged PR should be uninstalled
before using this version; import their credentials with `enroll --config <file>`.
On Windows, the earlier global `\Memoh\Runtime` task must be removed manually
after checking its Principal; this version only manages `\Memoh\Runtime-<SID>`.

POSIX enrollment files are private (0600). Windows files receive protected ACLs
before credentials are written. Atomic writes use a private staging directory.
Under `~/.memoh`, directories writable by other accounts, user-owned symlinks,
and reparse points are rejected without changing shared directory permissions;
ACL entries granted to the current user are fine. Node is pinned to its resolved
installation path and only has to be a regular file that other accounts cannot
rewrite, so package-manager prefixes such as Homebrew's admin-writable Cellar or
a shared `/usr/local` are accepted. If that Node installation is removed, for
example by a package upgrade, `service start` reports the missing path;
reinstall the runtime using an available Node executable.
