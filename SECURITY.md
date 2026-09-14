# Security

## What this tool does, and what it doesn't

`monad-failover` runs as root on a Monad full node during a validator migration.
It is a single static binary built from this repository with no third-party
modules; every import is from the Go standard library. Concretely, it:

- reads `KEYSTORE_PASSWORD` from `/home/monad/.env` (by parsing the one line, not
  by executing the file) and reads your `secp-backup` / `bls-backup` files
- writes only under `/home/monad/monad-bft/config`, `/opt/monad/backup` and
  `/var/lib/monad-failover` (resume state and staging), plus `/opt/monad/failover-logs`
- manages only the `monad-bft`, `monad-execution` and `monad-rpc` systemd units
- runs `monad-keystore`, `monad-sign-name-record`, `monad-status` and
  `systemctl` with argument vectors, never through a shell
- makes three kinds of outbound requests, all HTTPS: `ifconfig.me` to detect
  the server's public IPv4 (bypassed with `--public-ip`); Monad Foundation's
  validator snapshot (`bucket.monadinfra.com/validator-data/<network>.json`) to
  read the last published name record sequence for your key, which is used only
  to suggest a number you can override; and the monval uptime API
  (`validator-api.huginn.tech`, operated by Huginn) after cutover for a historical
  view of the validator identity. The lookup includes its public SECP key in
  the URL. As with any HTTPS request, the endpoint also sees the caller's IP.
  Snapshot and uptime failures do not block migration; failed IP detection
  requires an explicit `--public-ip` before signing can continue
- reads `/proc/net/tcp` and `/proc/net/tcp6` in the dry run to report RPC
  ports listening on non-loopback addresses

It contains no telemetry and never transmits your keys or password anywhere.
The process runs with umask `077`, so every file it creates (key backups,
resume state, staging, logs) is never world-readable, and the keystore
password is never written alongside the encrypted keystores.

`secp-backup` and `bls-backup` contain **unencrypted secret IKM**, both when you
provide them and when the tool exports fresh copies after cutover. Anyone with
those files can reconstruct the validator keys without the keystore password.
Their private file permissions are not encryption. Store off-server copies in
an encrypted vault and restrict access to copies left on the node. The separate
`failover-<timestamp>/` directory holds this target's original encrypted
keystores and config; it is not a backup of the incoming validator identity.

## One honest caveat: process arguments

The Monad key tools take the password and IKM as command-line flags
(`monad-keystore --password ... --ikm ...`). While those child processes run, their
arguments are visible in `/proc/<pid>/cmdline` to any local user. This is a property
of the Monad CLI, not something this tool can avoid. On a validator you should
already treat local access as full compromise, but if you want defence in depth,
mount `/proc` with `hidepid=2` so process arguments are not readable across users.

The tool itself never echoes or logs the password or IKM: manual IKM entry is
read with terminal echo off, and the output of every key-tool invocation that
carries a secret on its command line is discarded or parsed, never written to the
run log, so even an error path that echoed its arguments could not land there.

## A second caveat: secrets in memory

The IKM typed or read from a backup is held in a buffer this code zeroes after
the import. The copy that Go makes to pass it to `monad-keystore` as an argument
is a string the garbage collector owns, and it cannot be zeroed on request. The
same holds for the keystore password. Nothing here is written to disk or swap
by the tool, but a memory dump of the process while a key command runs could
contain those values. This is weaker than the shell release, which could clear
its variables, and it is stated here rather than papered over.

## Resume state is a root trust boundary

A resume run reads its saved state back and acts on it while running as root:
the state names the sequence number to sign, the IP to publish, and the
directory a failed step tells the operator to restore from. Whoever can write
that file can steer the run, so the file must not be writable by anyone but root.

For that reason the state lives in `/var/lib/monad-failover`, owned `root:root`
and mode `0700`, with the state file itself `0600`. It is not under
`/home/monad`, which the unprivileged `monad` service account owns. The install
instructions place the binary in root-owned `/usr/local/bin` on the same
grounds. On startup the tool:

- refuses to run if the state directory is not owned by root, or if the
  directory or the state file is a symlink, or the state file is not a regular
  file (an unprivileged user could otherwise pre-stage a symlink to redirect a
  root write); an existing directory whose mode is not exactly `0700` is
  refused, never repaired and then trusted;
- writes state atomically through a uniquely named temporary file created
  inside that directory (`os.CreateTemp`, `O_EXCL`), fsynced and renamed over
  the old file, never a predictable name;
- validates every field it reads back against a narrow shape before use: the
  step counter is a closed `1`–`8` range, not just digits, because an
  out-of-range value would satisfy every completed-step check and skip the
  whole migration to a fake completion; a key that appears twice in the file
  is refused rather than read as its first line;
- refuses, rather than silently migrating, any state left by an older version
  under `/home/monad/.monad-failover`: that path is writable by the `monad`
  account, so it is treated as untrusted and left in place for you to inspect;
- refuses the test-only overrides `MF_STATE_DIR`, `MF_IP_URL` and
  `MF_UPTIME_API_BASE` in a live (root) run instead of ignoring them, so a
  stray variable can never be assumed honoured.

Do not remove resume state just to bypass a refusal. If it cannot safely be
resumed, follow [manual recovery](docs/recovery.md) before starting a fresh run.

## How a cutover cannot leave a half-swapped node

The three files that make up this node's identity (`id-secp`, `id-bls` and
`node.toml`) cannot be swapped in one atomic step, so the run is built so that
every point it can be interrupted is recoverable:

- Staging lives in `/var/lib/monad-failover/staging`, owned by root and mode
  `0700`, not in the config directory. That directory belongs to the `monad`
  account, so anything staged there could be replaced between the checksum
  check and the rename.
- Each staged file is checksummed when it is created and you confirm it. The
  recorded value is never refreshed from disk, so a file changed after your
  confirmation is refused rather than accepted as the new expected content.
  Symlinks and non-regular files are refused.
- Placement copies the verified content into the destination directory, checks
  it again there, and only then renames it, so the final step is a rename
  within one filesystem and cannot be interrupted half-written. A move from the
  staging filesystem straight to the config filesystem would be a copy, not an
  atomic rename. The temporary file is created and written in a single open
  with `O_CREAT|O_EXCL|O_WRONLY` at mode `0600`: creating it and then reopening
  it by name would leave a window in which the name could be replaced with a
  symlink and the write would follow it, and no checksum afterwards can undo a
  write to the wrong file. The file is fsynced before the rename. The live file
  is checked once more after the rename.
- One boundary is not fully closed and is worth stating plainly: the
  destination directory belongs to the `monad` account, so the rename target is
  a path that account can manipulate. The single-open write removes the
  write-through-a-symlink hazard, and the check after the rename means a
  substitution is detected and the run stops before the services start, but
  a rename by file descriptor is not available, so detection rather than
  prevention is the guarantee for that last step. Operators who want the
  hazard gone entirely can make the config directory itself root-owned, with
  the `monad` account holding read and execute only.
- The units are masked before the swap and unmasked only once every file is in
  place. A plain stop is not enough: the units are normally enabled, so a reboot
  between two renames would otherwise bring the node up with a mixed identity.
  The mask is persistent, because a `--runtime` mask does not survive a reboot,
  and the run verifies the mask actually took effect before touching anything.
  What was already masked is recorded before the first mask is applied, so an
  interruption in between cannot make a resume read this run's own masks as
  yours. Units you had masked yourself are left masked, and not started.
- Placing the files and bringing the services up are two separately resumable
  stages. An interruption after the swap resumes into the bring-up, so the units
  cannot be left masked with nothing to unmask them. An unmask that does not
  take effect is never recorded as done.
- Before the units are unmasked and started, all three live files are checked
  against the recorded checksums again. The swap may have happened in an earlier
  run, and anything that changed in between must not be started: the run stops
  and points at the backup instead.
- The whole live run holds an exclusive `flock`. A second run is refused before
  it reads or writes any state.
- Once a cutover has begun that fact is recorded, and a later run without
  `--resume` refuses to start fresh over an unfinished swap.

`--resume` reads the recorded stage and continues from it: before the mask,
between renames, after the swap but before the services start, and during final
verification.

## Reading the Foundation snapshot

The sequence suggestion comes from Monad Foundation's published validator data.
The document is decoded as a whole with the standard JSON decoder; a truncated
response, or one with data after the document, is refused before any of it is
used. The snapshot's network and chain id must be the ones this node is on and
its timestamp must be recent. The entry must match your SECP key exactly and be
unique, and its BLS key must match the key you imported. A validator with no
published record is reported as unknown, never as sequence zero, and a sequence
outside the range a JSON number carries intact is refused. Anything that fails
these checks falls back to entering the number yourself, with the reason shown.

## Services active is not the same as validating

After the swap, consensus and execution must report active. RPC must also be
active unless it was already masked before this run; that operator choice is
preserved. Missing required services fail with recovery steps. Sync is a
separate question and gets a bounded window of its own. If the
node has not caught up in that window the run reports the cutover as complete
with verification pending, keeps the resume state, and does not print success.
The monval uptime figure is a 24 hour window keyed on the public key. It does
not prove that this new server is currently participating. Missing fields or
an inactive result are advisory and do not prevent key backup export.

## Verifying what you run

- Compare `sha256sum /usr/local/bin/monad-failover` against the checksum in the
  README. The build is reproducible: with Go 1.24.7 on linux/amd64, the command
  in the README's "Build from source" section yields the same bytes. CI builds
  the release the same way and fails any change where the README checksum and
  the build drift apart, and the release workflow refuses to publish a binary
  whose checksum differs from the tagged README.
- Run `monad-failover --dry-run` first. It performs every preflight check
  read-only and changes nothing.
- The code is about 3,700 lines under `cmd/` and `internal/`. The parts that
  touch secrets, root-owned state and the live node are `internal/state`
  (resume record), `internal/place` (checksummed placement), `internal/monad`
  (the key tools and signer), `internal/netinfo`, `internal/foundation` and
  `internal/uptime` (the three network requests) and `internal/promote` (the
  flow). `go vet ./...` and `go test ./...` run without root, network or
  systemd.

## Reporting a vulnerability

Report vulnerabilities privately via
[GitHub security advisories](https://github.com/s0urledd/monad-failover-tool/security/advisories/new)
rather than public issues. Reports are acknowledged on a best-effort basis, and
key-handling issues are treated as top priority.
