# Changelog

## 4.1.0 — 2026-07-10

- `--dry-run`: read-only preflight that runs every check (commands, files,
  sync, RPC exposure, key backup files, config flags) and changes nothing.
- `--version` flag.
- RPC exposure check now covers 8080, 8081, 8545, 8546, 9545, 9546, 18545, 18546.
- Post-migration reminder about the VDP metrics-push requirement.
- CI: ShellCheck (style level), syntax check, README-checksum consistency,
  flag smoke tests.
- Added SECURITY.md and this changelog.

## 4.0.0 — 2026-07-10

- Rebuilt as a single-purpose tool: promote a synced full node to validator,
  following the official node migration procedure. `prepare-standby` and
  `restore-fullnode` modes removed.
- Key backup files (`secp-backup` / `bls-backup`) as the primary key source,
  with IKM extraction — works when the old server is unreachable. Manual
  hidden IKM entry kept as the alternative (`--backup-dir` to skip the prompt).
- Takes over the old validator's `node_name` during migration (optional).
- Re-exports official-format key backups from the live keys after cutover,
  preserving previous files with a timestamp suffix.
- Kept: staging keys until cutover, sync gate, host confirmation,
  old-validator-stopped check (`--peer-host`), monotonic `seq_num`,
  per-step `--resume`.

## 3.0.0

- Three-mode toolkit: `promote`, `prepare-standby`, `restore-fullnode` (historical).
