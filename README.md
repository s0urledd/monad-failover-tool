# monad-failover

[![ci](https://github.com/s0urledd/monad-failover-tool/actions/workflows/ci.yml/badge.svg)](https://github.com/s0urledd/monad-failover-tool/actions/workflows/ci.yml)
[![go](https://img.shields.io/badge/Go-00ADD8?logo=go&logoColor=white)](go.mod)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Promotes a synced Monad full node to a validator, following the official
[node migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.
Use it for a planned migration or recovery when the old server is unavailable.
It runs on the target full node using your validator key backups, with no
connection to the old server required.

2.0.0-rc.1 is a release candidate under validation on testnet. Do not use it
on a mainnet validator before 2.0.0.

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

Prebuilt binary (linux/amd64), verified against the checksum in this README.
Run as root on the target full node:

```bash
curl -fsSLO https://github.com/s0urledd/monad-failover-tool/releases/download/v2.0.0-rc.1/monad-failover &&
echo "fb73459d4850339c76407fdb8e42581bfea798b7c7cc6b1c41ba78ed636aaa38  monad-failover" | sha256sum -c - &&
install -m 755 monad-failover /usr/local/bin/monad-failover
```

From source, with Go 1.24.7. The build is reproducible and gives the identical
binary and checksum:

```bash
git clone https://github.com/s0urledd/monad-failover-tool && cd monad-failover-tool
git checkout v2.0.0-rc.1
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o monad-failover ./cmd/monad-failover
sha256sum monad-failover
install -m 755 monad-failover /usr/local/bin/monad-failover
```

Or, with Go installed, `go install github.com/s0urledd/monad-failover-tool/cmd/monad-failover@v2.0.0-rc.1`;
the Go checksum database verifies the source and the binary lands in
`$(go env GOPATH)/bin`.

The checksum is verified before installation. If the download or verification
fails, your existing installation stays unchanged. CI builds the release the
same way and fails any change where the README checksum and the build drift
apart. The binary is static and needs no runtime; the commands it calls are
`systemctl`, `monad-keystore`, `monad-sign-name-record` and, for sync checks,
`monad-status`.

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

## Compatibility and operator notes

Maintained to track Monad updates. The tool targets standard P2P ports:
TCP/UDP `8000` and authenticated UDP `8001`. Custom P2P ports are not supported.

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

[MIT licensed](LICENSE).
