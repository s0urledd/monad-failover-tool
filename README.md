# monad-failover

Promotes a synced Monad full node to a validator, following the official
[node migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.
Built for the case where the old validator server is gone: all it needs is a synced full
node and your `secp-backup` / `bls-backup` key files (created during the official
[full node installation](https://docs.monad.xyz/node-ops/full-node-installation#generate-keystores) —
keep copies off-server).

## Install

```bash
cd /home/monad
curl -sSLO https://raw.githubusercontent.com/s0urledd/monad-failover-tool/main/monad-failover.sh
chmod +x monad-failover.sh

sha256sum monad-failover.sh
# 49f80f32bdd1684769d6faa0fa2eedf6df56981094de54d3da9f963418d0e067
```

## Usage

On a full node synced to the tip, with your backup files copied over:

```bash
./monad-failover.sh
```

| Flag | Effect |
|---|---|
| `--backup-dir PATH` | where `secp-backup` / `bls-backup` live; skips the key-source prompt |
| `--peer-host user@host` | check over SSH that the old validator is actually stopped |
| `--resume` | pick up where a previous run left off |

Fully interactive; asks for confirmation before anything irreversible. Keys are read
from the backup files by default, or pasted as raw IKM values (hidden input).

## How it works

1. Verifies the node is in-sync and you're on the right host; backs up the full node's
   own identity to `/opt/monad/backup/failover-<timestamp>/`.
2. Imports the validator keys into staging files (`id-secp.new` / `id-bls.new`) and
   shows the public keys for confirmation — live keys stay untouched until cutover.
3. Sets `node_name`, `beneficiary` and the required flags, signs a new name record
   with `self_record_seq_num` = previous + 1, patches `node.toml`.
4. After you confirm the old validator is stopped or down: stops services, swaps the
   keys in, restarts as validator, and re-exports fresh backup files from the live keys.

## Notes

- Never start the old machine again with the same keys — two nodes signing with one
  identity is the one mistake you can't undo.
- `self_record_seq_num` is monotonic; peers reject stale values. If you don't know the
  last value, enter anything you're sure is higher.
- The VDP requires validators to push metrics to Monad Foundation's monitoring
  infrastructure — set it up on the new server after migrating
  ([docs](https://docs.monad.xyz/node-ops/validator-delegation-program)).
- If downstream full nodes peer with this validator, update its name record in their
  `node.toml`.

MIT licensed.
