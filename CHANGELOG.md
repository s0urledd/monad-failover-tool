# Changelog

## 1.3.1 — 2026-07-14

- Closing-output cleanup: NODE STATUS is now just `journalctl -fu monad-bft`
  and VALIDATOR EVENTS just the `monad-ledger-tail` grep, both unindented for
  clean copy-paste. The pre-cutover warning block is shorter: one stop command,
  no verification sub-steps.

## 1.3.0 — 2026-07-14

### Fixed
- **Root cause of the stale-seq ghost node, found in a live mainnet migration:**
  `--node-config` made `monad-sign-name-record` read `self_record_seq_num` from
  the current node.toml (stale on a fresh full node) and ignore the
  `--self-record-seq-num` argument, signing a stale seq that peers reject.
  The signer is now invoked without `--node-config`, exactly like the official
  install-guide example: the seq comes from the argument, the pubkey from the
  keystore. One seq source end to end (argument → signer output → node.toml).
- New guard against signer version drift: if the emitted seq is LOWER than
  requested the run aborts before anything is written; if higher (a
  +1-incrementing version) it warns and continues, since the signature matches
  the emitted value.

### Changed
- **The seq prompt now asks for the final value, used verbatim.** Previously the
  script asked for the "last used" seq and added 1 internally; combined with the
  signer's own behaviour this stacked two layers of math on one number. Now you
  enter exactly what gets signed and written (last was 7 → enter 8). No value is
  read from or compared against the local node.toml: on a fresh full node that
  history does not exist — the real last seq lives in your records or on the old
  validator. Zero and non-numeric input are rejected.

### Added
- **Run logs.** Every live run is recorded to
  `/opt/monad/failover-logs/failover-<timestamp>.log` (dir mode 700). Output
  never contains secrets, so the log is safe to keep; until now the operator's
  only record of a migration was their terminal scrollback. Dry-run stays
  fully read-only and is not logged.

### Tests
- The signer mock now rejects `--node-config` outright, signs exactly the seq
  passed, and supports `MOCK_SEQ_OFFSET` to simulate version drift. New tests
  cover the warn-and-continue and abort-on-lower guard paths plus log-file
  creation (18 total).

## 1.2.1 — 2026-07-11

### Fixed
- **seq_num / signature mismatch.** `monad-sign-name-record` (v0.14.5) emits
  `self_record_seq_num` incremented from the value passed in, and the signature
  is bound to the emitted value. The script wrote its own computed seq into
  `node.toml` while the signature belonged to the signer's seq, producing a
  record peers cannot verify. All three fields (`self_address`,
  `self_record_seq_num`, `self_name_record_sig`) are now parsed from the signer
  output as the single source of truth; the promotion summary and resume state
  carry the signer's value. The test mock now mirrors the real +1 behaviour so
  this class of bug cannot reappear silently. Found during a live migration.

## 1.2.0 — 2026-07-11

### Added
- **Post-cutover uptime verification.** Step 8 now queries the
  [monval](https://monval.huginn.tech/) validator uptime API with the promoted
  public key and prints how the network sees the validator: status, 24h uptime,
  finalized/timeout counts and last round. A failed or not-yet-indexed lookup
  only warns with the URL to check later; it never blocks the run. The final
  output prints the API URL and the `monad-ledger-tail` live-events command
  instead of explorer links.

### Removed
- The co-located-services check (axelard/tofnd/vald/nginx). It targeted Axelar
  setups that do not occur on a Monad node, so it was only noise.
- The "Current SECP" line from the host-confirmation prompt. Showing the full
  node's throwaway key told the operator nothing; hostname and public IP are
  the identifying facts, and those stay.

## 1.1.0 — 2026-07-10

Security and robustness pass after a full audit of 1.0.0.

### Changed / removed
- **Removed `--peer-host`.** The SSH check treated an unreachable host as "stopped",
  which was less safe than the manual path for the one check that prevents
  double-signing. Cutover now requires the operator to type `STOPPED` after being
  shown exactly how to stop and verify the old validator.

### Fixed
- **Cutover is now crash-safe.** Both staging keys are checked before services are
  stopped, moves are guarded, and if the services fail to start after the key swap the
  run records the step as complete so `--resume` never repeats the (now impossible)
  swap. Recovery commands are printed on failure.
- **Keystore password is no longer written next to the encrypted keys.** The separate
  password-backup file and the `.env` copy were removed from the per-run backup
  directory.
- **Secret files are created with a `077` umask**, closing the create-then-chmod window
  where backups were briefly world-readable. Backup directories are `700`.
- **`.env` is parsed, not sourced.** Reading the keystore password no longer executes
  the file as root.
- **node.toml writes are injection-safe.** `beneficiary` (`0x` + 40 hex) and `node_name`
  are validated, sed replacements are escaped, and every write is verified.
- **Public IP detection uses HTTPS** and validates the result as an IPv4 address.
- Key-backup export is atomic (temp + rename); a failure preserves the previous copy
  and tells you where it is.
- Network detection and resume-state values are validated more strictly.

### Tests
- Added coverage for resume-after-cutover-failure, the staging-key guard, the `STOPPED`
  confirmation, beneficiary validation, and manual IKM entry.

## 1.0.0 — 2026-07-10

First public release.

**What it does:** promotes a synced Monad full node to a validator, following the
official [node migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration)
procedure, built for the case where the old validator server is unreachable.

- Key backup files (`secp-backup` / `bls-backup`) as the primary key source, with
  automatic IKM extraction; manual hidden IKM entry as the alternative
  (`--backup-dir` to skip the prompt).
- Staging keys (`id-secp.new` / `id-bls.new`): live keys untouched until cutover.
- `--dry-run` read-only preflight.
- Monotonic `self_record_seq_num`, name-record signing, `node.toml` patching,
  optional `node_name` takeover.
- Full-node identity backed up before overwrite; fresh key backups re-exported after
  cutover.
- RPC exposure check, per-step `--resume`, VDP metrics reminder.
- CI: ShellCheck, README-checksum gate, and a mocked end-to-end test suite (bats).
