# monad-failover

[![ci](https://github.com/s0urledd/monad-failover-tool/actions/workflows/ci.yml/badge.svg)](https://github.com/s0urledd/monad-failover-tool/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Promotes a synced Monad full node to a validator, following the official
[node migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.
Use it for a planned migration or recovery when the old server is unavailable.
It runs on the target full node using your validator key backups, with no
connection to the old server required.

Used in a successful Huginn mainnet migration, with no missed blocks observed.
See the [validation record](docs/validation.md).

## How it works

The full node keeps running while the tool backs up its identity and prepares
the validator keys and signed config in protected staging. You confirm the
public keys, beneficiary and sequence number.

Stop the old validator yourself, or confirm it is fully offline, then type
`STOPPED`. Only then does cutover mask and stop the target services, verify and
place the prepared files, and start the validator. The tool checks service
health and sync before exporting fresh key backups. Pending sync is reported
explicitly. RPC exposure is reported at the end, beside the VDP reminder.

## What you need

- A synced full node with the standard Monad setup. Its
  `/home/monad/.env` must contain its own `KEYSTORE_PASSWORD`; imported keys
  are encrypted with that password.
- Your validator's `secp-backup` and `bls-backup`: the text backups containing
  the secret IKM, not the encrypted `id-secp` / `id-bls` keystores.
  Copy them into a root-only directory on the target, for example `/root/validator-keys`
  (directory mode `700`, files `600`). Keep off-server copies; these are
  unencrypted secrets. Hidden manual IKM entry is also available.
- The validator's SECP and BLS public keys to compare at the confirmation prompt.
- Its beneficiary address and `node_name`, from your saved validator config.
  A blank beneficiary keeps the target's existing address only after confirmation.

The Foundation snapshot suggests a sequence when it has a matching record.
Use a number higher than every sequence this identity has used, even if that
exceeds the suggestion. Without a usable record, enter the number yourself.

## Install

Run as root on the target full node:

```bash
curl -fsSLO https://raw.githubusercontent.com/s0urledd/monad-failover-tool/v1.9.5/monad-failover.sh &&
echo "3d478c3be39468608bb5a9b5b0e989c5cb512a3daaec365902a67d39ac3c05b3  monad-failover.sh" | sha256sum -c - &&
install -m 755 monad-failover.sh /usr/local/bin/monad-failover
```

The checksum is verified before installation. If the download or verification
fails, your existing installation stays unchanged.

## Run

Use the directory holding the validator's backups for both commands:

```bash
monad-failover --dry-run --backup-dir /root/validator-keys
monad-failover --backup-dir /root/validator-keys
```

Dry-run checks prerequisites and backup format; it does not verify validator
identity or perform signing. Without `--backup-dir`, the live run asks where
the keys are. Its default `/opt/monad/backup` may contain the full node's own backups.

| Flag | Effect |
|---|---|
| `--dry-run` | Read-only preflight |
| `--backup-dir PATH` | Directory containing the validator's two backup files |
| `--public-ip IP` | Use this IPv4 instead of automatic detection |
| `--resume` | Continue an interrupted run |
| `--version` | Print the tool version |

## If a run is interrupted

Run `monad-failover --resume`, including after a partial cutover. Do not start
a fresh migration over an unfinished swap. Logs are in `/opt/monad/failover-logs/`.

The saved `/opt/monad/backup/failover-<timestamp>/` restores this server's
original full-node identity. It does not move the validator back to the old
server. See [recovery](docs/recovery.md) if resume cannot finish.

## Migration walkthrough

![Mainnet migration screenshot replay](docs/mainnet-migration.gif)

The still images reflect the updated flow; the GIF shows the earlier run.

<details>
<summary>View the updated terminal walkthrough</summary>

![Preflight and validator key import](docs/mainnet-run-1.png)

![Beneficiary, sequence and name record signing](docs/mainnet-run-2.png)

![Cutover and service verification](docs/mainnet-run-3.png)

![Completion and fresh backups](docs/mainnet-run-4.png)

</details>

## Compatibility and operator notes

Compatible with current Monad mainnet and testnet releases. Maintained to track
Monad updates.

The tool targets standard P2P ports: TCP/UDP `8000` and authenticated UDP `8001`.
Custom P2P ports are not supported.

- Keep the old validator offline after cutover. Never run both with the same keys.
- Block public access to RPC and metrics ports (8080, 8081, 9143, etc.).
  Allow trusted sources only.
- Configure [VDP metrics](https://docs.monad.xyz/node-ops/validator-delegation-program)
  on the target and check its firewall.
- Update downstream peers with the new name record. Transfer any custom
  dedicated-full-node configuration separately; the tool edits the target's config.

[SECURITY.md](SECURITY.md) explains key handling and external requests, including
the optional monval uptime lookup operated by Huginn.

## Uninstall

After verification, `sudo rm -- /usr/local/bin/monad-failover` removes the tool.
Monad, backups and logs stay in place. Keep the backups for recovery.

MIT licensed.
