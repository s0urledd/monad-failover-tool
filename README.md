# monad-failover

[![ci](https://github.com/s0urledd/monad-failover-tool/actions/workflows/ci.yml/badge.svg)](https://github.com/s0urledd/monad-failover-tool/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Promotes a synced Monad full node to a validator, following the official
[node migration](https://docs.monad.xyz/node-ops/node-recovery/node-migration) procedure.
It is built for the worst case: the old validator server is gone. All it needs is a
synced full node and your `secp-backup` / `bls-backup` key files (created during the
official [full node installation](https://docs.monad.xyz/node-ops/full-node-installation#generate-keystores);
keep copies off-server).

## Install

```bash
cd /home/monad
curl -sSLO https://raw.githubusercontent.com/s0urledd/monad-failover-tool/main/monad-failover.sh
chmod +x monad-failover.sh

sha256sum monad-failover.sh
# fc4712cb97e4e65a81727a3db01cb9a9b386cde4e2ba69308896722bf3db42bb
```

## Usage

On a full node synced to the tip, with your backup files copied over, start with a
dry run. It checks everything and changes nothing:

```bash
./monad-failover.sh --dry-run   # read-only preflight
./monad-failover.sh             # live run
```

| Flag | Effect |
|---|---|
| `--dry-run` | run every preflight check read-only; touch nothing |
| `--backup-dir PATH` | where `secp-backup` / `bls-backup` live; skips the key-source prompt |
| `--resume` | pick up where a previous run left off |
| `--version` | print version and exit |

The run is fully interactive and asks for confirmation before anything irreversible.
Keys are read from the backup files by default, or pasted as raw IKM values (hidden
input).

## How it works

1. Verifies the node is in-sync and that you are on the right host, then backs up the
   full node's own identity to `/opt/monad/backup/failover-<timestamp>/`.
2. Imports the validator keys into staging files (`id-secp.new` / `id-bls.new`) and
   shows the recovered public keys for confirmation. Live keys stay untouched until
   cutover.
3. Sets `node_name`, `beneficiary` and the required flags, signs a new name record
   with the `self_record_seq_num` you enter (the final value, used verbatim: last
   was 7 → enter 8), and patches `node.toml`.
4. Asks you to confirm the old validator is stopped (you type `STOPPED`), then cuts
   over: stops services, swaps the keys in, restarts as validator, and re-exports
   fresh backup files from the live keys.
5. Verifies the result: local sync status, plus a query to the
   [monval](https://monval.huginn.tech/) uptime API so you see how the network sees
   your validator (status, 24h uptime, finalized/timeout counts) right in the output.

If the services fail to start after the swap, the run records the cutover as done so
`--resume` never repeats it, and prints the exact commands to recover. Every live run
is also recorded to `/opt/monad/failover-logs/` (no secrets ever appear in the output),
so you always have a log of what happened.

## Why trust a shell script with your validator keys?

Fair question. The mitigations, in the order they matter:

1. **One auditable file.** No dependencies to vet beyond the Monad binaries and
   coreutils. Read it before you run it; it is short.
2. **Dry-run first.** `--dry-run` exercises every check without touching a file, key
   or service.
3. **Checksum-pinned.** The README hash must match the script, and CI fails otherwise.
4. **Tested end-to-end.** CI runs the full promotion flow (including cutover failure
   and resume) against mocked Monad binaries with bats, plus ShellCheck, on every commit.
5. **Nothing sensitive leaves the machine.** No telemetry. The only outbound calls
   are HTTPS requests to `ifconfig.me` (public-IP detection) and the monval uptime
   API (post-cutover check, public key only). Secrets are never logged, and secret
   files are created `600`. See [SECURITY.md](SECURITY.md) for the full surface.

## Notes

- Never start the old machine again with the same keys. Two nodes signing with one
  identity is the one mistake you cannot undo, which is why cutover requires you to
  type `STOPPED` after confirming the old validator is down.
- `self_record_seq_num` is monotonic and peers reject stale values. The number you
  enter is used as-is; it lives in your records or on the old validator, not on the
  new machine. If you do not know the last value, enter one you are sure is higher;
  gaps are harmless.
- The VDP requires validators to push metrics to Monad Foundation's monitoring
  infrastructure. Set that up on the new server after migrating
  ([docs](https://docs.monad.xyz/node-ops/validator-delegation-program)).
- If downstream full nodes peer with this validator, update its name record in their
  `node.toml`.

MIT licensed.
