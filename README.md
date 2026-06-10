# monad-failover

Move a Monad validator between machines with minimal downtime.
Follows the official [Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.

## Modes

| Mode | What it does |
|---|---|
| `promote` | Turn a **synced** full node into the validator |
| `prepare-standby` | Sync a box as a full node under **temporary** keys |
| `restore-fullnode` | Return a box to its **own** full-node identity |

## Install

```bash
cd /home/monad
curl -sSLO https://raw.githubusercontent.com/s0urledd/monad-failover-tool/main/monad-failover.sh
chmod +x monad-failover.sh

sha256sum monad-failover.sh
# 525c979ab4c3b0a39f3c3ad9fad42dfc8b7b5a4bebfe30ae2cf3258bc4aa02b4  monad-failover.sh
```

## Usage

```bash
./monad-failover.sh promote              # synced standby → validator
./monad-failover.sh prepare-standby      # fresh box → synced full node
./monad-failover.sh restore-fullnode     # old box → original full-node identity
./monad-failover.sh <mode> --resume      # continue after interruption
```

**Flags:**

- `--peer-host user@host` — *(promote)* SSH check that the old validator is stopped
- `--snapshot-reset` — *(prepare-standby)* hard-reset from snapshot
- `--resume` — continue from last completed step

## How a full move works

```
prepare-standby              promote                    restore-fullnode
fresh box ──────▶ synced full node ──────▶ validator    old box ──────▶ full node
(temp keys)        (temp keys)    (cutover) (val keys)
```

1. **`prepare-standby`** on the target — syncs under temporary keys
2. Wait for `monad-status` → **`in-sync`**
3. **`promote`** — imports validator keys to staging, confirms old validator stopped, swaps keys in at cutover
4. **`restore-fullnode`** on the old box *(optional)* — restores full-node identity

## Safety

- **No double-sign by design** — validator keys are imported to staging files (`id-secp.new`/`id-bls.new`); live keys are only swapped in at cutover after services are stopped and old validator is confirmed down
- **In-sync gate** — `promote` refuses unless synced
- **Run-location guard** — hostname, IP, current key displayed before confirmation
- **Active validator guard** — `prepare-standby` refuses if current key is in `validators.toml`
- **Backup before overwrite** — keys, config, keystore password backed up to `/opt/monad/backup/`
- **IKM validation** — rejects wrong key formats before import (SECP: 64 hex, BLS: 0x + 64 hex)
- **Key handling** — IKM entered hidden, never logged or written to state
- **Co-located service protection** — only touches `monad-bft` / `monad-execution` / `monad-rpc`
- **Resume** — per-step state file, continues after SSH drops or interruptions

## seq_num

`self_record_seq_num` is per-keypair, monotonic, increments on every migration. Peers reject stale values.

Example round trip: `1` (original) → `2` (failover) → `3` (back).

## Troubleshooting

**`Invalid format ... "Odd number of digits"`** — Template placeholders crash `monad-sign-name-record`. The tool sanitizes these automatically. Only affects `prepare-standby`.

**`ChecksumError` / password mismatch** — Keystores are encrypted with the creating box's password. The tool imports from IKM (re-encrypts with local password). `restore-fullnode` restores the original password from backup.

**`dropping proposal, randao validation failed`** — SECP/BLS mismatch. Re-import with the correct IKM without `--key-type` (known BLS corruption bug).

**`Epoch not found in validator_map`** — Outdated `validators.toml`. Restart services or use `--snapshot-reset`.

**Far behind (> ~600 blocks)** — Statesync triggers automatically. Use `--snapshot-reset` only if statesync stalls.

## Requirements

- Monad >= 0.14.4 (mainnet) / >= 0.14.5 (testnet)
- `monad-keystore`, `monad-sign-name-record` on PATH
- `/home/monad/.env` with `KEYSTORE_PASSWORD`
- Synced full node (for `promote`)
- `monad-status` recommended (not required)
- `aria2c` required only for `--snapshot-reset`
- `openssl` required for `prepare-standby`

## Reference

- [Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration)
- [Full Node Installation](https://docs.monad.xyz/node-ops/full-node-installation)
- [Hard Reset](https://docs.monad.xyz/node-ops/node-recovery/hard-reset)

## License

MIT
