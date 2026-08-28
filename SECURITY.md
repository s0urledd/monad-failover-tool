# Security

## What this script does, and what it doesn't

`monad-failover.sh` runs as root on a Monad full node during a validator migration.
It is a single auditable file with no dependencies beyond the Monad binaries and
standard system tools. Concretely, it:

- reads `KEYSTORE_PASSWORD` from `/home/monad/.env` (by parsing the one line, not
  by sourcing the file as code) and reads your `secp-backup` / `bls-backup` files
- writes only under `/home/monad/monad-bft/config`, `/opt/monad/backup` and
  `/var/lib/monad-failover` (resume state)
- manages only the `monad-bft`, `monad-execution` and `monad-rpc` systemd units
- makes two kinds of outbound requests, both HTTPS: `ifconfig.me` to detect the
  server's public IP, and the monval uptime API (`validator-api.huginn.tech`) after
  cutover to confirm the network sees the validator. Only the public key is sent;
  a failed API call never blocks the run

It contains no telemetry and never transmits your keys or password anywhere. Secret
files it creates (key backups, resume state) are created with a `077` umask so they
are never world-readable, and the keystore password is never written alongside the
encrypted keystores.

## One honest caveat: process arguments

The Monad key tools take the password and IKM as command-line flags
(`monad-keystore --password ... --ikm ...`). While those child processes run, their
arguments are visible in `/proc/<pid>/cmdline` to any local user. This is a property
of the Monad CLI, not something this script can avoid. On a validator you should
already treat local access as full compromise, but if you want defence in depth,
mount `/proc` with `hidepid=2` so process arguments are not readable across users.

The script itself never echoes or logs the password or IKM: manual IKM entry is
hidden (`read -s`), IKM shell variables are cleared right after use, and the
output of every key-tool invocation that carries a secret on its command line is
suppressed, so even an error path that echoed its arguments could not land in
the run log.

## Resume state is a root trust boundary

A resume run reads its saved state back and acts on it while running as root:
the state names the sequence number to sign, the IP to publish, and the
directory a failed step tells the operator to restore from. Whoever can write
that file can steer the run, so the file must not be writable by anyone but root.

For that reason the state lives in `/var/lib/monad-failover`, owned `root:root`
and mode `0700`, with the state file itself `0600`. It is not under
`/home/monad`, which the unprivileged `monad` service account owns. On startup the script:

- refuses to run if the state directory is not owned by root, or if the
  directory or the state file is a symlink, or the state file is not a regular
  file (an unprivileged user could otherwise pre-stage a symlink to redirect a
  root write);
- writes state atomically through a `mktemp` file inside that directory, never a
  predictable `.tmp` name;
- validates every field it reads back against a narrow allowlist before use. In
  particular the step counter is range-checked to 1-8 before it reaches an
  arithmetic expansion, because Bash arithmetic evaluates an array subscript and
  a subscript runs command substitution, so an unchecked value there would
  execute as root;
- refuses, rather than silently migrating, any state left by an older version
  under `/home/monad/.monad-failover`: that path is writable by the `monad`
  account, so it is treated as untrusted and left in place for you to inspect.

If you see one of these refusals on a machine only you administer, it usually
means an interrupted run and a stale file: restore the node from
`/opt/monad/backup` if needed, remove the reported path, and start a fresh run.

## Verifying what you run

- Compare `sha256sum monad-failover.sh` against the checksum in the README. CI
  fails any change where the two drift apart.
- Run `./monad-failover.sh --dry-run` first. It performs every preflight check
  read-only and changes nothing.
- The script is short enough to read before running. Please do.

## Reporting a vulnerability

Report vulnerabilities privately via
[GitHub security advisories](https://github.com/s0urledd/monad-failover-tool/security/advisories/new)
rather than public issues. Reports are acknowledged on a best-effort basis, and
key-handling issues are treated as top priority.
