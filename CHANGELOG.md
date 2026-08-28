# Changelog

## 1.9.0 — 2026-08-28

Security hardening of the resume state, which a live run reads back and acts on
while running as root.

- **State moved to a root-owned location.** Resume state now lives in
  `/var/lib/monad-failover` (`root:root`, `0700`; state file `0600`) instead of
  `/home/monad/.monad-failover`. The old path is under `$MONAD_HOME`, writable by
  the unprivileged `monad` service account — anyone who could write it could steer
  a root run.
- **Injection fix.** The step counter read from state reached an arithmetic
  expansion without validation. Bash arithmetic evaluates an array subscript and a
  subscript runs command substitution, so a crafted value executed as root (the
  `set -u` guard only caught the naive form). Every field read from state is now
  checked against a narrow, format-specific allowlist before use, the step counter
  is range-checked (`1-8`, so an out-of-range value can no longer skip the whole
  migration), and each step's required fields must be present before a resume acts
  on them.
- **Symlink and ownership guards.** The script refuses to run if the state
  directory is not root-owned, or if the directory or state file is a symlink or
  the state file is not a regular file, and writes state atomically through a
  `mktemp` file rather than a predictable `.tmp` name.
- **A loose state directory is refused, not repaired.** An existing state
  directory must already be `0700` (and root-owned in a live run); the script no
  longer relaxes-then-trusts one that was left group- or world-writable, since its
  contents may have been planted.
- **Cutover/state consistency is enforced** before both a resume and a fresh-run
  decision: a `cutover_started` flag requires the three staged checksums and a
  step of 6-8, and reaching step 7+ requires the flag. This closes a path where a
  truncated state file could offer "start fresh" after a cutover had already run.
- **Legacy state is refused, not migrated.** State left by an older version under
  `/home/monad/.monad-failover` is treated as untrusted: the run stops and asks
  you to inspect and remove it rather than resuming from it.
- **`MF_STATE_DIR` override is test-only.** It is honoured only alongside
  `MF_ALLOW_NONROOT`; a live root run rejects it so the state location cannot be
  redirected to a user-writable path.
- Bounded the two outbound `curl` calls with `--max-time` and a response-size cap.
- Tests: 17 new cases cover the injection (a payload that bypasses `set -u`),
  range and presence checks, symlinked dir/file, non-root ownership, refusal of a
  loose-permission state dir, legacy-state refusal, the `MF_STATE_DIR` guard,
  `backup_dir` escaping the backup root, `network`/`beneficiary` shape, the
  cutover-consistency rules, and permission handling. The suite runs unprivileged.

## 1.8.0 — 2026-07-28

- **New monad-sign-name-record CLI.** Upstream replaced `--address ip:port`
  with separate `--ip`, `--tcp-port` and `--udp-port` flags; the signer is
  now invoked with the new form. The old form is not kept around: the change
  ships with a hard fork, so there is no version to stay compatible with.
- Tests: the signer mock mirrors the new CLI and rejects `--address`, so a
  regression back to the old invocation fails the suite.

## 1.7.1 — 2026-07-16

- The RPC exposure warning no longer talks past operators who already run a
  deny-by-default firewall: a listening socket behind ufw is fine, and the
  message now says so instead of implying action is always required.

## 1.7.0 — 2026-07-16

Fixes from an independent re-audit of 1.6.1 (two adversarial passes: security
and correctness). No key-loss path existed before or after; all changes are
about failure-path recovery and log hygiene.

### Fixed
- **An interrupted cutover is now always resumable.** Previously, any failure
  after services were stopped (a rename failing mid-swap, or a crash between
  the swap and the service start) wedged the operator: `--resume` refused
  because a staging file was consumed, while a fresh run could not pass the
  sync check with services down — and the printed advice pointed in circles.
  Now the checksums of the staged files are recorded before anything moves;
  on a re-run, a missing staging file is accepted exactly when the live file
  matches what was staged, so `--resume` finishes only the remaining part of
  the swap. The swap is recorded as complete before services are started,
  and every cutover failure message now points at `--resume`.
- **A fresh run is refused while an interrupted cutover exists.** Starting
  fresh after a partial swap would have re-snapshotted a possibly mixed
  identity as "this server's previous identity" and pointed later recovery
  messages at that corrupted backup. Once cutover begins the run is marked,
  and until it is finished (or the operator deliberately deletes the state
  file after a manual restore) only `--resume` is accepted.
- **A failed key-backup export no longer discards the resume state.** The
  export failure message said "re-run with --resume", but the run then
  completed and cleared its state, so `--resume` would have started a full
  fresh promotion against the live validator — with the default key source
  already renamed to `.bak`. The run now fails with the state kept at the
  cutover step, so `--resume` re-checks health and retries only the export.
- **Secret-bearing key-tool output can no longer reach the run log.** The
  run log (added in 1.3.0) captures stderr, and two of the four key-tool
  calls that carry the IKM or keystore password on their command line did
  not suppress their output; a CLI error path echoing its arguments would
  have persisted a secret to disk. Both now suppress output, matching the
  other two calls, and their failure messages say so explicitly.
- `.env` files saved with CRLF line endings no longer produce a corrupted
  keystore password (the trailing carriage return defeated quote-stripping).
- If `ss` is not installed, the RPC exposure check now says it cannot check
  instead of printing a false "not exposed" all-clear. `sha256sum` joined the
  required-commands preflight (it verifies placed files during resume).

### Tests
- The mock `monad-status` now reports in-sync only while the mock services
  are running, like real hardware — this fidelity gap is what had hidden the
  wedge above from the suite. New mocks let a single rename or a single key
  export fail on demand. Five new tests: partial cutover resumed to
  completion, crash-after-swap-before-start resumed, fresh run refused after
  an interrupted cutover, failed backup export retried via resume, and CRLF
  `.env` parsing (27 total).

## 1.6.1 — 2026-07-15

- Dry-run's planned-actions list updated to the staged-config flow (staging
  copy of node.toml, staged swap at cutover, post-cutover health check).
- Changelog wording made precise: the three staged files are swapped into
  place during cutover with individually guarded renames; the set is not one
  atomic operation.

## 1.6.0 — 2026-07-15

Hardening pass, round two (external review follow-up).

- **node.toml is now staged too.** Beneficiary, node_name, config flags and the
  signed name record are all written to `node.toml.new`; the live config is
  swapped into place during cutover together with the keys. Aborting at the
  STOPPED gate (or anywhere before cutover) leaves the running full node
  byte-for-byte unmodified — previously the live config was already mutated.
- **Hard post-cutover health gate.** `systemctl start` returning success is not
  trusted: after a short wait every service must report active, or the run
  fails with recovery steps and keeps its resume state. A unit that crashes
  right after starting can no longer produce a false
  "VALIDATOR PROMOTION COMPLETE".
- **Self-verifying install.** The README install now pipes the pinned hash
  through `sha256sum -c`, which prints OK or fails loudly — no eyeballing.

Tests: 22 total. New: abort-at-STOPPED leaves the live node untouched
(keys AND config), crash-after-start is caught then resume completes, and the
resume-after-start-failure path now asserts the health gate blocks resume
until services are actually up.

## 1.5.1 — 2026-07-15

- Battle-tested section in the README with a full render of the live mainnet
  migration run (docs/mainnet-run.png).
- Dry-run's planned-actions list updated to the current seq semantics (the
  value you enter is used verbatim; the old "previous + 1" wording was stale).

## 1.5.0 — 2026-07-15

Hardening pass from an external review.

- **Install is pinned to a release tag.** The README now downloads
  `v<version>/monad-failover.sh` instead of mutable `main`, so what users fetch
  never changes after the fact. CI gained a gate that fails if the README's
  install URL stops matching the script's version.
- **`--public-ip` flag** to set the name-record address explicitly instead of
  auto-detecting via ifconfig.me (multi-homed hosts, or when the detector is
  unreachable). IPv4 input is validated per octet, as is the detected address.
- **Explicit root check** for live runs, with a clear message (dry-run stays
  usable without root).
- **Resume-state writes need no escaping**: rewrite-then-rename replaces the
  sed edit, so unusual characters in paths or signatures cannot corrupt state.
- README: dependency wording corrected (bash, curl, systemd and standard text
  tools, not just coreutils) and a "Tested versions" section added
  (Monad v0.14.5, live mainnet migration).

## 1.4.0 — 2026-07-15

TUI restyle. The flow, prompts, ordering and messages are unchanged; only the
presentation is new.

- Phase banners with a step counter (`━━ [4/8] VALIDATOR KEY IMPORT ━━…`) so
  the operator always knows where they are and how much is left.
- Compact header box with right-aligned version; boxed, column-aligned
  PROMOTION SUMMARY; green double-rule completion banner.
- Uniform `? label › ` input prompts; sub-steps use a lighter `▸` marker.
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
