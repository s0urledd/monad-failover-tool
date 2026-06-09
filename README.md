# monad-failover

A single bash script to move a Monad validator between machines with minimal downtime,
following the official
[Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.

It handles key import, name-record signing, `node.toml` patching, the service cutover,
and cleanup — with guards against the mistakes that are easy to make under pressure
(wrong box, double-sign, stale config, touching the wrong services).

## Modes

| Mode | What it does |
|---|---|
| `promote` | Turn an **already-synced** full node into the validator |
| `prepare-standby` | Sync a box as a full node under **temporary** keys, ready for `promote` |
| `restore-fullnode` | Return a box to its **own** full-node identity from backup |

## Install

```bash
cd /home/monad
curl -sSLO https://raw.githubusercontent.com/s0urledd/monad-failover-tool/main/monad-failover.sh
chmod +x monad-failover.sh

sha256sum monad-failover.sh
# e0c2caa574122fce5deec8ab0f99ae37382d87ac739743f44b89cbb12159cf1c  monad-failover.sh
```

## Quick reference

| Situation | Run |
|---|---|
| Validator down, backup is **synced** | `promote` on the backup |
| Validator down, backup **not synced** | `prepare-standby` → wait in-sync → `promote` |
| **Move back** to the original box | `prepare-standby` on original → `promote` → `restore-fullnode` on temp box |
| Return a box to plain **full node** | `restore-fullnode` on that box |

## Usage

```bash
./monad-failover.sh promote              # on the synced standby, at cutover
./monad-failover.sh prepare-standby      # on a fresh/behind box
./monad-failover.sh restore-fullnode     # on the old box, afterwards
./monad-failover.sh <mode> --resume      # continue after interruption
```

**Flags:**

- `--peer-host user@host` — *(promote)* SSH to verify/stop the old validator during cutover
- `--snapshot-reset` — *(prepare-standby)* hard-reset from snapshot for a far-behind box
- `--resume` — continue from last completed step. State: `~/.monad-failover/<mode>/state`

## How a full move works

```
  prepare-standby              promote                    restore-fullnode
  fresh box ──────▶ synced full node ──────▶ validator    old box ──────▶ full node
  (temp keys)        (temp keys)    (cutover) (val keys)
```

1. **`prepare-standby`** on the target — syncs under temporary keys (no conflict with the live validator)
2. Wait for `monad-status` → **`in-sync`**, `blockDifference: 0`
3. **`promote`** — imports validator keys, signs name record (seq + 1), stops old → starts new. Downtime = seconds between stop and start
4. **`restore-fullnode`** on the old box *(optional)* — restores its full-node identity, overwrites the validator key copy

No snapshot or hard-reset during `promote` — the node is already synced.

## What each mode does

### `promote`

1. Check in-sync (refuses otherwise), warn if RPC 8080 exposed, show co-located services
2. Confirm hostname/IP — guard against running on the wrong box
3. Back up keys + `node.toml` + `.env` to `/opt/monad/backup/`
4. Import validator SECP + BLS from IKM (hidden), recover and display pubkeys for confirmation
5. Set `beneficiary`, `self_record_seq_num` (prev + 1), ensure config flags
6. Sign name record, patch `[peer_discovery]`, verify writes
7. Confirm old validator stopped (or `--peer-host` SSH stop), mask publishing timers, cutover
8. Verify in-sync

Keeps the existing `node.toml` and patches in place — does **not** download a fresh template (avoids the placeholder crash).

### `prepare-standby`

1. Confirm target host, back up any existing config
2. Generate temporary SECP/BLS keys (random, not your validator keys)
3. Download full-node `node.toml` template, set burn beneficiary, unique `node_name`
4. Verify `.env` has `REMOTE_VALIDATORS_URL` / `REMOTE_FORKPOINT_URL`
5. Sanitize template placeholders, sign name record (seq 1), patch
6. `--snapshot-reset`: hard-reset + restore from snapshot. Otherwise statesync handles catch-up
7. Start services, verify status

### `restore-fullnode`

1. Require Monad services stopped, select backup from `/opt/monad/backup/`
2. Restore `id-secp` / `id-bls` / `node.toml` (overwrites validator keys — security cleanup)
3. Restore original `KEYSTORE_PASSWORD` from backup
4. Recover + confirm pubkey is the full-node identity (not validator)
5. Sign name record (backup seq + 1), patch
6. Start as full node, verify in-sync

## seq_num

`self_record_seq_num` is per **keypair**, monotonic, and increments on every migration
regardless of physical box. Peers reject stale or equal values.

- `promote`: previous validator seq + 1
- `prepare-standby`: fresh temp key starts at 1
- `restore-fullnode`: backup seq + 1

Example round trip: `1` (original) → `2` (failover) → `3` (back). The temp key has its own counter.

## Safety

- **In-sync gate** — `promote` refuses unless synced
- **Double-sign guard** — `promote` blocks until old validator confirmed stopped
- **Run-location guard** — hostname + IP + current key displayed, explicit confirmation required
- **Co-located service protection** — only touches `monad-bft` / `monad-execution` / `monad-rpc`
- **Backup before overwrite** — every mode backs up keys, config, keystore password, and IKM backups
- **IKM format validation** — rejects wrong key formats before import (SECP: 64 hex no 0x, BLS: 0x + 64 hex)
- **Key handling** — IKM entered hidden, never logged or written to state
- **File ownership** — sets `monad:monad` and `chmod 600` on key files after every mutation
- **Placeholder sanitization** — rewrites template dummies to valid hex before signing
- **Write verification** — confirms `sed` actually wrote the expected values
- **Resume** — per-step state, continues after SSH drops or network blips

## Troubleshooting

**`Invalid format ... "Odd number of digits"` when signing** — Fresh `node.toml` templates
have placeholder values (`<NAME_RECORD_SIG>`, `<IP>:<PORT>`) that crash
`monad-sign-name-record`. The tool sanitizes these before signing. Only affects
`prepare-standby`; `promote` patches the existing valid config.

**`ChecksumError` / password mismatch** — Keystores are encrypted with the creating box's
`KEYSTORE_PASSWORD`. The tool imports from IKM (re-encrypts with local password).
`restore-fullnode` restores the original password from backup.

**`dropping proposal, randao validation failed`** — SECP/BLS key mismatch. Two common
causes: (1) BLS IKM was imported with `--key-type` flag (known bug — corrupts the keystore),
or (2) wrong IKM was used. Fix: re-import using the correct IKM without `--key-type`, then
restart `monad-bft`. The tool already uses the correct import command (no `--key-type`).

**`Epoch not found in validator_map`** — Outdated `validators.toml`. If close to the
network tip, restart services (soft reset). If far behind, use `--snapshot-reset`.

**`RaptorCastSecondary rejecting invite with group size exceeds max`** — Set
`max_group_size` in `[fullnode_raptorcast]` to match the value in the error log, then
hard-reset.

**Far behind (> ~600 blocks)** — Statesync triggers automatically. Use
`prepare-standby --snapshot-reset` only if statesync stalls or the triedb panics
(`result_ptr.is_null()`).

**`state root doesn't match, are peers trusted?`** — Version mismatch. Check
`dpkg -l | grep monad` and update to the latest release.

## After a move

Downstream full nodes must update the validator's name record in their `node.toml`.

## Requirements

- Synced Monad full node (for `promote`)
- `monad-keystore`, `monad-sign-name-record` on PATH
- `/home/monad/.env` with `KEYSTORE_PASSWORD`
- `monad-status` recommended (not required)
- `aria2c` required only for `--snapshot-reset`
- `openssl` required for `prepare-standby` (temp key generation)

## Reference

- [Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration)
- [Full Node Installation](https://docs.monad.xyz/node-ops/full-node-installation)
- [Hard Reset](https://docs.monad.xyz/node-ops/node-recovery/hard-reset)

## License

MIT
