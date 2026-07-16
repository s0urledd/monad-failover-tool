# Security

## What this script does, and what it doesn't

`monad-failover.sh` runs as root on a Monad full node during a validator migration.
It is a single auditable file with no dependencies beyond the Monad binaries and
standard system tools. Concretely, it:

- reads `KEYSTORE_PASSWORD` from `/home/monad/.env` (by parsing the one line, not
  by sourcing the file as code) and reads your `secp-backup` / `bls-backup` files
- writes only under `/home/monad/monad-bft/config`, `/opt/monad/backup` and
  `/home/monad/.monad-failover` (resume state)
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
