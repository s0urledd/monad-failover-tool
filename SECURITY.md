# Security

## What this script does — and doesn't

`monad-failover.sh` runs as root on a Monad full node during a validator migration.
It is a single auditable file with no dependencies beyond the Monad binaries and
standard system tools. Concretely, it:

- reads `/home/monad/.env` and your `secp-backup` / `bls-backup` files locally
- writes only under `/home/monad/monad-bft/config`, `/opt/monad/backup` and
  `/home/monad/.monad-failover` (resume state)
- manages only the `monad-bft`, `monad-execution` and `monad-rpc` systemd units
- makes exactly two kinds of outbound requests: `ifconfig.me` for public-IP
  detection, and an optional SSH check to a host you name with `--peer-host`

It never transmits keys or secrets anywhere, never logs or echoes IKM values or
the keystore password, and contains no telemetry.

## Verifying what you run

- Compare `sha256sum monad-failover.sh` against the checksum in the README —
  CI fails any change where the two drift apart.
- Run `./monad-failover.sh --dry-run` first: it performs every preflight check
  read-only and changes nothing.
- The script is short enough to read before running. Please do.

## Reporting a vulnerability

Report vulnerabilities privately via
[GitHub security advisories](https://github.com/s0urledd/monad-failover-tool/security/advisories/new)
rather than public issues. Reports are acknowledged on a best-effort basis;
key-handling issues are treated as top priority.
