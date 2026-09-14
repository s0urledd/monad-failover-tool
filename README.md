# monad-failover

[![ci](https://github.com/s0urledd/monad-failover-tool/actions/workflows/ci.yml/badge.svg)](https://github.com/s0urledd/monad-failover-tool/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Promotes a synced Monad full node to a validator, following the official
[node migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.
Use it for a planned migration or recovery when the old server is unavailable.
It runs on the target full node using your validator key backups, with no
connection to the old server required.

2.0.0-rc.1 is a Go implementation of the procedure and is being validated on
testnet. For a mainnet migration today use the
[1.9.5 shell release](#stable-release-195), which was used successfully on
Monad mainnet with v0.16.2. See the [validation record](docs/validation.md).

## How it works

1. With your synced full node and validator backups ready, the tool backs up
   the target's identity and imports the validator keys into protected staging.
   You confirm the public keys, beneficiary and sequence number, then the tool
   signs the name record and checks the staged config. The full node keeps running.
2. Preparation and signing are complete before the switch. Review the summary,
   stop the old validator (or ensure it is offline), and type `STOPPED`.
   When you are ready, confirm `proceed with cutover?` to start the switch.
3. The tool rechecks the prepared files, masks and stops the target services,
   places the validator keys and config, verifies the placed files, and starts
   the services. It then checks service health and sync and exports fresh key
   backups. Pending sync is reported explicitly.

## What you need

- A synced full node with the standard Monad setup and `KEYSTORE_PASSWORD`
  set in `/home/monad/.env`.
- Your validator's `secp-backup` and `bls-backup`: the text backups containing
  the secret IKM, not the encrypted `id-secp` / `id-bls` keystores.
  Place them in a private directory of your choice on the target
  (directory `700`, files `600`). These are unencrypted secrets; keep off-server
  copies. Hidden manual IKM entry is also available.
- The validator's SECP and BLS public keys to compare at the confirmation prompt.
- Its beneficiary address and `node_name`, from your saved validator config.
  A blank beneficiary keeps the target's existing address only after confirmation.

The Foundation snapshot suggests a sequence when it has a matching record.
Use a number higher than every sequence this identity has used, even if that
exceeds the suggestion. Without a usable record, enter the number yourself.

## Install

Run as root on the target full node (linux/amd64):

```bash
curl -fsSLO https://github.com/s0urledd/monad-failover-tool/releases/download/v2.0.0-rc.1/monad-failover &&
echo "5a0d3f4e9a2245f8a450146008ec4c3f9e7d1ea2854824bb98779fb356952006  monad-failover" | sha256sum -c - &&
install -m 755 monad-failover /usr/local/bin/monad-failover
```

The checksum is verified before installation. If the download or verification
fails, your existing installation stays unchanged. The binary is static and
needs no runtime; the commands it calls are `systemctl`, `monad-keystore`,
`monad-sign-name-record` and, for sync checks, `monad-status`.

## Run

Replace `/path/to/validator-backups` with your backup directory in both commands:

```bash
monad-failover --dry-run --backup-dir "/path/to/validator-backups"
monad-failover --backup-dir "/path/to/validator-backups"
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

A run interrupted under the 1.9.5 shell release can be finished with
`--resume` here: the state file has the same layout and location.

## Migration walkthrough

![Mainnet migration walkthrough](docs/mainnet-migration.gif)

The walkthrough follows the migration from preparation through confirmation
and completion. It was captured with the 1.9 shell release; apart from the
version in the banner, the terminal output is the same.

<details>
<summary>View the terminal walkthrough</summary>

![Preflight and validator key import](docs/mainnet-run-1.png)

![Beneficiary, sequence and name record signing](docs/mainnet-run-2.png)

![Cutover and service verification](docs/mainnet-run-3.png)

![Completion and fresh backups](docs/mainnet-run-4.png)

</details>

## Compatibility and operator notes

The 1.9.5 shell release was used successfully on Monad mainnet with v0.16.2.
2.0 performs the same procedure with the same prompts, state and safeguards;
its own live-network runs are recorded in the
[validation record](docs/validation.md) as they happen. Maintained to track
Monad updates.

The tool targets standard P2P ports: TCP/UDP `8000` and authenticated UDP `8001`.
Custom P2P ports are not supported.

- Block public access to RPC and metrics ports (8080, 8081, 9143, etc.).
  Allow trusted sources only.
- Configure [VDP metrics](https://docs.monad.xyz/node-ops/validator-delegation-program)
  on the target and check its firewall.
- Update downstream peers with the new name record. Transfer any custom
  dedicated-full-node configuration separately; the tool edits the target's config.

[SECURITY.md](SECURITY.md) explains key handling and external requests, including
the optional monval uptime lookup operated by Huginn.

## Build from source and verify

The release binary is reproducible. With Go 1.24.7 on linux/amd64:

```bash
git clone https://github.com/s0urledd/monad-failover-tool && cd monad-failover-tool
git checkout v2.0.0-rc.1
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w -buildid=' -o monad-failover ./cmd/monad-failover
sha256sum monad-failover
```

The checksum matches the one in the install command above and the
`checksums.txt` attached to the release. CI builds the same way and fails
any change where the README checksum and the build drift apart.
`go test ./...` runs the test suite without root, network or systemd.

## Uninstall

After verification, `sudo rm -- /usr/local/bin/monad-failover` removes the tool.
Monad, backups and logs stay in place. Keep the backups for recovery.

## Stable release (1.9.5)

The shell release stays available at its tag and is the version to use on
mainnet until 2.0.0 is validated. Its install command is unchanged:

```bash
curl -fsSLO https://raw.githubusercontent.com/s0urledd/monad-failover-tool/v1.9.5/monad-failover.sh &&
echo "1716029216ad46832b010bdff67f0d218e26393b22d3fb7e92de8195563bc7ba  monad-failover.sh" | sha256sum -c - &&
install -m 755 monad-failover.sh /usr/local/bin/monad-failover
```

It receives no new features. Its source and documentation are at
[v1.9.5](https://github.com/s0urledd/monad-failover-tool/tree/v1.9.5).

[MIT licensed](LICENSE).
