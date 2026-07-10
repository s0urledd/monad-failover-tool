# monad-failover

Promotes a synced Monad full node to a validator, following the official
[node migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.

This script exists for the day your validator machine dies. If you keep a synced full
node running and have your key backups stored off-server, you can move the validator
identity over and be signing again in minutes, without needing the old server at all.

## Install

```bash
cd /home/monad
curl -sSLO https://raw.githubusercontent.com/s0urledd/monad-failover-tool/main/monad-failover.sh
chmod +x monad-failover.sh

sha256sum monad-failover.sh
# 01b5df25d716f312733ad34ee16d89942b317e8aaae9912d8abdb835f12074dc
```

## Back up your keys now, not later

Everything depends on having your validator's key backup files available when the
machine is gone. If you followed the official
[full node installation](https://docs.monad.xyz/node-ops/full-node-installation#generate-keystores)
guide they already exist:

```
/opt/monad/backup/secp-backup
/opt/monad/backup/bls-backup
```

If not, re-create them on the validator:

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

Copy both files somewhere off the server (password manager, secrets vault). They contain
the private keys that *are* your validator identity: anyone holding them can take the
validator over, and without them you cannot migrate at all. Losing the keys means
re-registering with a new identity and moving every delegation by hand.

While you're at it, note down the current `self_record_seq_num` and `beneficiary`
values from the validator's `node.toml`. The script asks for both during failover.

## Running a failover

You need a full node synced to the tip (`monad-status` reports `in-sync`). Copy your
`secp-backup` and `bls-backup` files onto it, then:

```bash
./monad-failover.sh
```

The script walks through the procedure interactively and asks for confirmation before
anything irreversible.

| Flag | Effect |
|---|---|
| `--backup-dir PATH` | where `secp-backup` / `bls-backup` live; skips the key-source prompt |
| `--peer-host user@host` | check over SSH that the old validator is actually stopped |
| `--resume` | pick up where a previous run left off |

Keys can be provided two ways. The default is to read the backup files and extract the
IKM secrets from them, which works with the old server completely unreachable. You can
also paste the two IKM hex values by hand instead (input stays hidden).

What it does, in order:

1. Checks the node is in-sync, warns if RPC is publicly exposed, confirms you're on
   the right host.
2. Saves the full node's own keys and config to `/opt/monad/backup/failover-<timestamp>/`.
3. Imports the validator keys into staging files (`id-secp.new` / `id-bls.new`) and
   shows the public keys so you can verify them. Live keys are not touched yet.
4. Sets `node_name`, `beneficiary` and the required flags (`enable_publisher`,
   `enable_client`, `expand_to_group`).
5. Signs a new name record with `self_record_seq_num` = previous + 1 and patches
   `node.toml`.
6. Asks you to confirm the old validator is stopped (or checks via `--peer-host`),
   then cuts over: stop services, swap the keys into place, start services.
7. Verifies sync and re-exports fresh `secp-backup` / `bls-backup` files from the
   now-live keys.

## Design notes

- The validator keys never exist at the live path until cutover. Everything before
  that point can be aborted or re-run freely.
- Cutover happens only after services are stopped and you've confirmed the old
  validator is down. Never start the old machine again with the same keys — two nodes
  signing with one identity is the one mistake you can't undo.
- The full node's original identity is backed up before being replaced, so the machine
  can be turned back into its old full-node self by hand if needed.
- IKM values are format-validated and never echoed or written to logs.
- Only `monad-bft`, `monad-execution` and `monad-rpc` are managed; anything else
  running on the machine is left alone.
- Every step records its completion, so a dropped SSH session is a `--resume`, not
  a restart.

## seq_num

`self_record_seq_num` is monotonic per identity and peers reject anything stale. Give
the script the last value the identity used (0 if it has never migrated); it signs with
that plus one. If the old server is gone and you don't know the value, use anything you
are sure is higher.

## Troubleshooting

| Symptom | What it means |
|---|---|
| `Odd number of digits` when signing | placeholder values in `node.toml`; the script sanitizes them automatically |
| `ChecksumError` importing keys | `KEYSTORE_PASSWORD` in `.env` differs from the one the keystore was created with; the script re-encrypts from IKM under the current password |
| `randao validation failed` after cutover | the secp and bls backups were swapped — re-check which file is which |
| Public keys at the verify step look wrong | wrong backup files; answer no at the prompt and nothing is changed |

## Requirements

- A synced full node with `monad-keystore` and `monad-sign-name-record` in `PATH`
- `KEYSTORE_PASSWORD` in `/home/monad/.env`
- Your `secp-backup` / `bls-backup` files, or the raw IKM values
- `monad-status` for the sync check (recommended)

## Afterwards

- If other full nodes peer with this validator as a dedicated node
  (`[[fullnode_dedicated.identities]]`), update its name record in their `node.toml`.
- Get the freshly exported backup files off the server again.
- If the old machine comes back, wipe or re-key it before starting Monad services —
  see [restoring the original validator](https://docs.monad.xyz/node-ops/node-recovery/node-migration#restoring-the-original-validator).
  It must never run with the migrated keys.

MIT licensed.
