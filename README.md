# monad-failover

Promote a synced Monad full node to validator — one command, following the official
[Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.

Built for the worst case: the old validator server is dead and unreachable. As long as you
have your [key backup files](#before-you-need-it-export-your-key-backups) and a synced full
node, you can be back in consensus in minutes.

## Install

```bash
cd /home/monad
curl -sSLO https://raw.githubusercontent.com/s0urledd/monad-failover-tool/main/monad-failover.sh
chmod +x monad-failover.sh

sha256sum monad-failover.sh   # 01b5df25d716f312733ad34ee16d89942b317e8aaae9912d8abdb835f12074dc
```

## Before you need it: export your key backups

Do this **today, on your validator**, while it is healthy. If you followed the official
[full node installation](https://docs.monad.xyz/node-ops/full-node-installation#generate-keystores)
guide, the backup files already exist at `/opt/monad/backup/secp-backup` and
`/opt/monad/backup/bls-backup`. If they are missing, re-export them:

```bash
source /home/monad/.env

monad-keystore recover \
  --password "$KEYSTORE_PASSWORD" \
  --keystore-path /home/monad/monad-bft/config/id-secp \
  --key-type secp > /opt/monad/backup/secp-backup

monad-keystore recover \
  --password "$KEYSTORE_PASSWORD" \
  --keystore-path /home/monad/monad-bft/config/id-bls \
  --key-type bls > /opt/monad/backup/bls-backup
```

Store copies of both files **outside the server** (password manager or secrets vault).
They contain your validator's private keys — they *are* your validator identity. Anyone
with access can take it over; losing them means you cannot migrate, and re-registering
with a new identity requires moving all delegations manually.

Also record the current `self_record_seq_num` and `beneficiary` from the validator's
`node.toml` — you will be asked for them during failover.

## Failover

On a synced full node (see [full node installation](https://docs.monad.xyz/node-ops/full-node-installation)):

```bash
# Copy your secp-backup / bls-backup files onto the full node, then:
./monad-failover.sh
```

| Flag | Effect |
|---|---|
| `--backup-dir PATH` | Directory containing `secp-backup` / `bls-backup` (skips the key-source prompt) |
| `--peer-host user@host` | SSH-check that the old validator is stopped |
| `--resume` | Continue from the last completed step after an interruption |

### Providing the validator keys — two ways

1. **Key backup files** (recommended) — the tool extracts the IKM secrets from your
   `secp-backup` / `bls-backup` files automatically. Works even when the old server is
   completely unreachable.
2. **Manual IKM entry** — paste the two IKM hex values by hand (hidden input).

### What it does

1. Verifies the node is `in-sync`, checks RPC exposure, confirms the target host.
2. Backs up the full node's own keys and config to `/opt/monad/backup/failover-<timestamp>/`.
3. Imports the validator keys to **staging files** (`id-secp.new` / `id-bls.new`) and shows
   the recovered public keys for confirmation — live keys stay untouched.
4. Sets `node_name`, `beneficiary`, `enable_publisher`, `enable_client`, `expand_to_group`.
5. Signs a new name record (`self_record_seq_num` = previous + 1) and patches `node.toml`.
6. Confirms the old validator is stopped or down, then cuts over: stops services, swaps
   the keys into place, restarts as validator.
7. Verifies sync and re-exports fresh `secp-backup` / `bls-backup` files from the live keys.

## Safety

- **No double-sign by design** — keys are staged and swapped in only at cutover, after
  services stop and you confirm the old validator is down. Never restart the old server
  with the same keys afterward.
- **Backup before overwrite** — the full node's own identity is saved before anything changes.
- **Safe key handling** — IKM secrets are format-validated, never echoed or logged; only
  `monad-bft` / `monad-execution` / `monad-rpc` are ever touched.
- **Resumable** — per-step state survives SSH drops; re-run with `--resume`.

## seq_num

`self_record_seq_num` is per-identity and monotonic; peers reject stale values. Enter the
last value the identity used (0 if it never migrated) — the tool signs with previous + 1.
If you don't know it, enter a value you are certain is higher than the last one used.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `Odd number of digits` when signing | Template placeholders — sanitized automatically |
| `ChecksumError` on key import | `KEYSTORE_PASSWORD` in `.env` differs from the one used at import — the tool re-encrypts from IKM under the current password |
| `randao validation failed` | SECP/BLS backups swapped — check which file came from which key |
| Public keys shown don't match | Wrong backup files — abort at the confirmation prompt |

## Requirements

- A synced Monad full node with `monad-keystore` and `monad-sign-name-record` on `PATH`
- `/home/monad/.env` with `KEYSTORE_PASSWORD`
- Your validator's `secp-backup` / `bls-backup` files (or the raw IKM values)
- `monad-status` recommended for the sync gate

## After failover

- If downstream full nodes peer with this validator (`[[fullnode_dedicated.identities]]`),
  update the validator's name record in their `node.toml`.
- Store the freshly exported backup files off-server again.
- To return the old server to service later, follow
  [Restoring the original validator](https://docs.monad.xyz/node-ops/node-recovery/node-migration#restoring-the-original-validator) —
  never with the migrated keys while this validator is live.

## Reference

[Node Migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) ·
[Full Node Installation](https://docs.monad.xyz/node-ops/full-node-installation) ·
[Hard Reset](https://docs.monad.xyz/node-ops/node-recovery/hard-reset)

## License

MIT
