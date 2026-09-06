# monad-failover

[![ci](https://github.com/s0urledd/monad-failover-tool/actions/workflows/ci.yml/badge.svg)](https://github.com/s0urledd/monad-failover-tool/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Promotes a synced Monad full node to a validator, following the official
[node migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.
It is built for the worst case: the old validator server is gone. All it needs is a
synced full node and your validator key backups.

## What you need

- A full node synced to the tip, with its services running.
- Your `secp-backup` and `bls-backup` files, copied onto this server. They are
  created during the official
  [full node installation](https://docs.monad.xyz/node-ops/full-node-installation#generate-keystores);
  keep copies off-server. If you do not have them, you can paste the raw IKM
  values instead (hidden input).
- The beneficiary address from the old validator. If you leave it blank the
  tool shows the address already in this node's config and asks you to confirm it.

You do not need to look up the name record sequence number. The tool reads the
last published value for your key from Monad Foundation's validator snapshot and
suggests the next one. Press Enter to accept it, or type a higher number if you
know of a later one.

## Install

Pinned to a release tag, so what you download never changes after the fact:

```bash
curl -fsSLo /usr/local/bin/monad-failover \
  https://raw.githubusercontent.com/s0urledd/monad-failover-tool/v1.9.2/monad-failover.sh

echo "046b45df9d343a8bb82c09ca5546c864bf43a715bf0f31050850e4495a39b90a  /usr/local/bin/monad-failover" | sha256sum -c -
chmod 755 /usr/local/bin/monad-failover
```

`sha256sum -c` prints `/usr/local/bin/monad-failover: OK` and fails loudly on any
mismatch, so there is nothing to eyeball. It installs to root-owned
`/usr/local/bin` because it runs as root.

## Run

Start with a dry run. It checks everything and changes nothing:

```bash
monad-failover --dry-run   # read-only preflight
monad-failover             # live run
```

| Flag | Effect |
|---|---|
| `--dry-run` | run every preflight check read-only; touch nothing |
| `--backup-dir PATH` | where `secp-backup` / `bls-backup` live; skips the key-source prompt |
| `--public-ip IP` | use this IPv4 in the name record instead of auto-detecting |
| `--resume` | pick up where a previous run left off |
| `--version` | print version and exit |

## What you will see

The run is interactive and asks for confirmation before anything irreversible.
It goes through eight phases:

1. Sync and RPC checks, and a confirmation that you are on the right host.
2. This node's own identity is backed up to `/opt/monad/backup/failover-<timestamp>/`.
3. Your validator keys are imported to staging files, and their public keys are
   shown for you to confirm.
4. Beneficiary, node name and the required flags are set on a staging copy of
   `node.toml`.
5. The sequence number is suggested from the Foundation snapshot; you accept or
   override it.
6. The name record is signed and patched into the staging config.
7. You confirm the old validator is stopped by typing `STOPPED`, then the keys
   and config are swapped in and the services start as a validator.
8. Every service must come up active, then the node's sync status and the
   [monval](https://monval.huginn.tech/) uptime API are checked.

Nothing on the live node changes before phase 7. Abort at any prompt up to that
point and the full node is exactly as it was.

If the node has not caught up by the end of phase 8, the run says so plainly
instead of claiming success, and tells you the one command to re-check later.

![Preflight, host confirmation and config backup](docs/run-1.png)

![Validator key import from backup files](docs/run-2.png)

![Beneficiary, node name and seq_num configuration](docs/run-3.png)

![Name record signing and cutover](docs/run-4.png)

![Post-cutover verification and completion](docs/run-5.png)

## If a run is interrupted

Run `monad-failover --resume`. It works out how far the previous run got and
continues from there, including part-way through the cutover. It never repeats a
step that already completed, and it refuses to start a fresh run over an
unfinished cutover. Every live run is logged to `/opt/monad/failover-logs/`.

## Supported Monad versions

Verified against the name record signer shipped in monad **v0.16.1**, on a real
node using a throwaway key. That signer prints the address and each port on its
own line, while `node.toml` carries one combined `self_address = "IP:PORT"` plus
a separate `self_auth_port`, so the tool assembles the address itself and copies
the ports across. The exact output it was built against is kept in the test
suite as a fixture. If a future release changes the shape, the run stops before
anything is written rather than guessing.

## Battle-tested

Proven in a live mainnet migration on Monad v0.14.5 (July 2026): the Huginn
validator was moved to a fresh full node with this script. That run surfaced two
real signer behaviours which are fixed and regression-locked in the test suite.

## Notes

- Do not run the old and new machines with the same keys at the same time. Two
  nodes under one identity disrupt this validator's consensus participation and
  name record, which is why cutover makes you type `STOPPED` first.
- The VDP requires validators to push metrics to Monad Foundation's monitoring
  infrastructure. Set that up on the new server after migrating
  ([docs](https://docs.monad.xyz/node-ops/validator-delegation-program)).
- If downstream full nodes peer with this validator, update its name record in
  their `node.toml`.

How the tool protects your keys, what it verifies, and every network call it
makes are documented in [SECURITY.md](SECURITY.md).

MIT licensed.
