# monad-failover

Move a Monad validator between machines with minimal downtime, following the official
[Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.

| Mode | Purpose |
|---|---|
| `promote` | Turn a synced full node into the validator |
| `prepare-standby` | Sync a server as a full node under temporary keys |
| `restore-fullnode` | Return a server to its own full-node identity |

`prepare-standby` → `promote` is a full move; `restore-fullnode` returns the old server to a full node.

## Install

```bash
cd /home/monad
curl -sSLO https://raw.githubusercontent.com/s0urledd/monad-failover-tool/main/monad-failover.sh
chmod +x monad-failover.sh

sha256sum monad-failover.sh   # 54a33f89a2ca202424a158126823009318a00c3b726603f800061d890debfe8d
```

## Usage

```bash
./monad-failover.sh promote           # promote a synced full node to validator
./monad-failover.sh prepare-standby   # sync a standby server as a full node (temp keys)
./monad-failover.sh restore-fullnode  # return a former-validator server to full-node identity
./monad-failover.sh <mode> --resume   # continue after an interruption
```

| Flag | Mode | Effect |
|---|---|---|
| `--peer-host user@host` | promote | SSH-check the old validator is stopped |
| `--snapshot-reset` | prepare-standby | Hard-reset from snapshot instead of statesync |
| `--resume` | any | Continue from the last completed step |

## How a move works

```
Target server:  new  ──prepare-standby──▶  synced full node  ──promote (cutover)──▶  validator
Old server:      validator  ──restore-fullnode──▶  full node
```

1. `prepare-standby` syncs the target server under temporary keys.
2. Wait for `monad-status` → `in-sync`.
3. `promote` stages the validator keys, confirms the old validator is stopped, then swaps keys in at cutover.
4. `restore-fullnode` (optional) returns the old server to its full-node identity.

## Safety

- **No double-sign by design** — validator keys are staged to `id-secp.new`/`id-bls.new` and swapped into place only at cutover, after services stop and the old validator is confirmed down.
- **Guards before any destructive step** — in-sync gate on `promote`; host/identity confirmation; `prepare-standby` refuses if the live key is an active validator in `validators.toml`.
- **Backup before overwrite** — keys, `node.toml`, and keystore password saved to `/opt/monad/backup/`.
- **Safe key handling** — IKM entered hidden, format-validated, never logged; only `monad-bft`/`monad-execution`/`monad-rpc` are ever touched.
- **Resumable** — per-step state survives SSH drops and interruptions.

## seq_num

`self_record_seq_num` is per-keypair and monotonic; peers reject stale values. The tool sets
previous + 1 on each migration (e.g. a round trip runs `1 → 2 → 3`).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `Odd number of digits` when signing | Template placeholders — sanitized automatically |
| `ChecksumError` / password mismatch | Keystore password mismatch — tool re-encrypts from IKM; `restore-fullnode` restores the original |
| `randao validation failed` | SECP/BLS mismatch — re-import the correct IKM without `--key-type` |
| Far behind (> ~600 blocks) | Statesync runs automatically; use `--snapshot-reset` only if it stalls |

## Requirements

- A current Monad install with `monad-keystore` + `monad-sign-name-record` on `PATH`
- `/home/monad/.env` with `KEYSTORE_PASSWORD`; a synced full node for `promote`
- `monad-status` recommended; `aria2c` for `--snapshot-reset`; `openssl` for `prepare-standby`

## Reference

[Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) ·
[Full Node Installation](https://docs.monad.xyz/node-ops/full-node-installation) ·
[Hard Reset](https://docs.monad.xyz/node-ops/node-recovery/hard-reset)

## License

MIT
