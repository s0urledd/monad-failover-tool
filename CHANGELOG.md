# Changelog

## 1.0.0 — 2026-07-10

First public release.

**What it does:** promotes a synced Monad full node to a validator, following the
official [node migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration)
procedure — built for the case where the old validator server is unreachable.

- Key backup files (`secp-backup` / `bls-backup`) as the primary key source, with
  automatic IKM extraction; manual hidden IKM entry as the alternative
  (`--backup-dir` to skip the prompt).
- Staging keys (`id-secp.new` / `id-bls.new`): live keys untouched until cutover,
  which happens only after services stop and the old validator is confirmed
  stopped or down (`--peer-host` for an SSH check).
- `--dry-run`: read-only preflight that runs every check and changes nothing.
- Monotonic `self_record_seq_num` handling, name-record signing, `node.toml`
  patching with verification, optional `node_name` takeover per the docs.
- Full-node identity backed up before overwrite; fresh official-format key
  backups re-exported from the live keys after cutover (old files preserved
  with a timestamp suffix).
- RPC exposure check across 8080, 8081, 8545, 8546, 9545, 9546, 18545, 18546.
- Per-step `--resume`; secrets never logged or echoed; only `monad-bft`,
  `monad-execution` and `monad-rpc` are managed.
- Post-migration VDP reminder (metrics push to Monad Foundation monitoring).
- CI: ShellCheck (style), README-checksum consistency gate, and a mocked
  end-to-end promotion test suite (bats).
