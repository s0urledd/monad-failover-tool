# Security

## What this script does, and what it doesn't

`monad-failover.sh` runs as root on a Monad full node during a validator migration.
It is a single auditable file with no dependencies beyond the Monad binaries and
standard system tools. Concretely, it:

- reads `KEYSTORE_PASSWORD` from `/home/monad/.env` (by parsing the one line, not
  by sourcing the file as code) and reads your `secp-backup` / `bls-backup` files
- writes only under `/home/monad/monad-bft/config`, `/opt/monad/backup` and
  `/var/lib/monad-failover` (resume state)
- manages only the `monad-bft`, `monad-execution` and `monad-rpc` systemd units
- makes three kinds of outbound requests, all HTTPS and all optional to the
  result: `ifconfig.me` to detect the server's public IP; Monad Foundation's
  validator snapshot (`bucket.monadinfra.com/validator-data/<network>.json`) to
  read the last published name record sequence for your key, which is used only
  to suggest a number you can override; and the monval uptime API
  (`validator-api.huginn.tech`) after cutover to confirm the network sees the
  validator. Nothing but a public key is ever sent, and a failed call to any of
  them never blocks the run

It contains no telemetry and never transmits your keys or password anywhere. Secret
files it creates (key backups, resume state) are created with a `077` umask so they
are never world-readable, and the keystore password is never written alongside the
encrypted keystores.

## One honest caveat: process arguments

The Monad key tools take the password and IKM as command-line flags
(`monad-keystore --password ... --ikm ...`). While those child processes run, their
arguments are visible in `/proc/<pid>/cmdline` to any local user. This is a property
of the Monad CLI, not something this script can avoid. On a validator you should
already treat local access as full compromise, but if you want defence in depth,
mount `/proc` with `hidepid=2` so process arguments are not readable across users.

The script itself never echoes or logs the password or IKM: manual IKM entry is
hidden (`read -s`), IKM shell variables are cleared right after use, and the
output of every key-tool invocation that carries a secret on its command line is
suppressed, so even an error path that echoed its arguments could not land in
the run log.

## Resume state is a root trust boundary

A resume run reads its saved state back and acts on it while running as root:
the state names the sequence number to sign, the IP to publish, and the
directory a failed step tells the operator to restore from. Whoever can write
that file can steer the run, so the file must not be writable by anyone but root.

For that reason the state lives in `/var/lib/monad-failover`, owned `root:root`
and mode `0700`, with the state file itself `0600`. It is not under
`/home/monad`, which the unprivileged `monad` service account owns. The install
instructions place the script itself in root-owned `/usr/local/bin` on the same
grounds. On startup the script:

- refuses to run if the state directory is not owned by root, or if the
  directory or the state file is a symlink, or the state file is not a regular
  file (an unprivileged user could otherwise pre-stage a symlink to redirect a
  root write);
- writes state atomically through a `mktemp` file inside that directory, never a
  predictable `.tmp` name;
- validates every field it reads back against a narrow allowlist before use. In
  particular the step counter is range-checked to 1-8 before it reaches an
  arithmetic expansion, because Bash arithmetic evaluates an array subscript and
  a subscript runs command substitution, so an unchecked value there would
  execute as root;
- refuses, rather than silently migrating, any state left by an older version
  under `/home/monad/.monad-failover`: that path is writable by the `monad`
  account, so it is treated as untrusted and left in place for you to inspect.

If you see one of these refusals on a machine only you administer, it usually
means an interrupted run and a stale file: restore the node from
`/opt/monad/backup` if needed, remove the reported path, and start a fresh run.

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
  with `O_CREAT|O_EXCL`: creating it and then reopening it by name would leave
  a window in which the name could be replaced with a symlink and the write
  would follow it, and no checksum afterwards can undo a write to the wrong
  file. It is created `0600` by the script's umask, so there is no
  chmod-by-path either. The live file is checked once more after the rename.
- One boundary is not fully closed and is worth stating plainly: the
  destination directory belongs to the `monad` account, so the rename target is
  a path that account can manipulate. The single-open write removes the
  write-through-a-symlink hazard, and the check after the rename means a
  substitution is detected and the run stops before the services start, but a
  shell cannot rename by file descriptor, so detection rather than prevention
  is the guarantee for that last step. Operators who want the hazard gone
  entirely can make the config directory itself root-owned, with the `monad`
  account holding read and execute only.
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
It is read with a structural pass that tracks string state and brace depth, so
every field is taken from inside the object it belongs to, and the whole
document must be balanced before any of it is used. Object boundaries alone are
not enough: a response truncated after the target object would otherwise still
parse and yield a sequence. A flat text scan
would attribute a neighbouring validator's sequence to your key as soon as the
publisher reorders fields. The entry must match your SECP key exactly and be
unique, its BLS key must match the key you imported, and the snapshot's network
and chain id must be the ones this node is on. A validator with no published
record is reported as unknown, never as sequence zero. Anything that fails these
checks falls back to entering the number yourself, with the reason shown.

## Services active is not the same as validating

After the swap, every unit must report active or the run fails with the recovery
steps. Sync is a separate question and gets a bounded window of its own. If the
node has not caught up in that window the run reports the cutover as complete
with verification pending, keeps the resume state, and does not print success.
The monval uptime figure is a 24 hour window keyed on the public key, so it says
the identity is participating; it is not on its own proof that this new server is
the one doing it.

## Verifying what you run

- Compare `sha256sum /usr/local/bin/monad-failover` against the checksum in the
  README. CI fails any change where the README and the repo script drift apart.
- Run `monad-failover --dry-run` first. It performs every preflight check
  read-only and changes nothing.
- The script is short enough to read before running. Please do.

## Reporting a vulnerability

Report vulnerabilities privately via
[GitHub security advisories](https://github.com/s0urledd/monad-failover-tool/security/advisories/new)
rather than public issues. Reports are acknowledged on a best-effort basis, and
key-handling issues are treated as top priority.
