# Changelog

## 2.0.0-rc.1 — 2026-09-14

First release candidate.

- Single static linux/amd64 binary built from the Go standard library only.
  The build is reproducible; the README carries the release checksum and CI
  fails when the two drift apart.
- Eight-phase migration: preflight, host confirmation, backup of the target's
  identity, validator key import into protected staging, configuration on a
  staging copy, name record signing, cutover behind two typed confirmations,
  verification with fresh key backups. Every phase is resumable.
- Safeguards: units masked before the swap so a reboot cannot start a
  half-swapped node; checksums recorded at confirmation and never refreshed
  from disk; each file placed through a single `O_EXCL` open, verified before
  and after the rename; root-owned state with every field validated before
  use; an exclusive lock for the whole run; a started cutover refuses a fresh
  run.
- Run-time requirements: `systemctl`, `monad-keystore`,
  `monad-sign-name-record` and, for sync checks, `monad-status`.
- Test suite of 104 functions, including in-process flow tests that drive the
  full migration against mock Monad binaries and inject faults between file
  placements. Runs without root, network or systemd.
- Secrets are held as bytes and zeroed after use; free memory is returned to
  the kernel after every command that carried a secret; the process cannot
  dump core, is not dumpable and, as root, is never paged out to swap.
- Not yet used on a live network.

Releases before 2.0 were a shell script; their history is at the `v1.9.5` tag.
