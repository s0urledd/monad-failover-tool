# Go implementation (2.0.0-dev)

This branch carries a Go implementation of the same migration the shell
release performs. It is development work, not a release. `monad-failover.sh`
v1.9.5 remains the supported tool; the README install instructions and
checksum refer to it and are unchanged here.

## Build

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o monad-failover ./cmd/monad-failover
```

The result is a static amd64 binary of about 6 MB with no third-party
modules; every import is from the Go standard library. Monad validators run
on amd64, so no other target is built.

## What is the same

- The eight phases, their prompts, confirmations and messages. Recovery
  instructions printed on failure are word for word those of the shell
  release, so `docs/recovery.md` applies to both.
- The state file: same directory (`/var/lib/monad-failover`), same
  `key=value` layout, same field rules. A run interrupted under 1.9.5 can be
  finished with the Go binary's `--resume`, and the reverse. Do not run both
  at once; the lock refuses a second run whichever implementation holds it.
- The safety design: units masked before the swap; checksums recorded at
  confirmation and never refreshed from disk; each file placed through a
  single `O_CREAT|O_EXCL` open at 0600, verified, renamed within its
  directory and verified again; root-owned state with symlink, mode and
  owner checks; every state field validated against a narrow shape before
  use; an exclusive lock for the whole run; a started cutover refuses a
  fresh run.
- Standard P2P ports 8000/8001 in the signed record; custom ports out of scope.
- Required versus advisory results: consensus and execution must be active;
  a unit the operator had masked stays masked and is not started; snapshot,
  uptime and IP detection failures degrade with an explicit path.
- Command-line flags and environment overrides (`MONAD_HOME`, `BACKUP_ROOT`,
  `LOG_DIR`, `FOUNDATION_DATA_BASE`, `FOUNDATION_MAX_AGE`, `MF_HEALTH_WAIT`,
  `MF_SYNC_WAIT`, and in the test sandbox `MF_ALLOW_NONROOT`, `MF_STATE_DIR`).

## What is different, on purpose

- HTTP requests use `net/http`. `curl`, `sed`, `sha256sum` and `flock` are no
  longer needed at run time; the required commands are `systemctl`,
  `monad-keystore` and `monad-sign-name-record`.
- The Foundation snapshot is read with `encoding/json`. The shell release
  carried a hand-written structural JSON reader in awk; the same checks
  (complete document, network, chain id, freshness, unique key match, BLS
  match, no record is not sequence zero, sane range) are applied to the
  decoded document.
- RPC exposure is read from `/proc/net/tcp` and `/proc/net/tcp6` rather than
  `ss`. Loopback addresses, including IPv4-mapped IPv6 loopback, do not warn.
- Library code returns errors; only `main` prints and exits. This is what
  lets the test suite run the whole migration in-process and inject faults
  such as a rename failing between two placements.
- The public IP is detected once per run and reused; the shell release
  asked the service twice.
- State writes and placed files are fsynced before the rename.
- Two endpoint overrides exist for the test suite only, `MF_IP_URL` and
  `MF_UPTIME_API_BASE`. Like `MF_STATE_DIR`, a live (root) run refuses them
  rather than ignoring them.
- The root-gate message names the binary instead of "this script".

## What is weaker, stated plainly

Secrets in memory. The shell release cleared its IKM variables after use.
In Go, values that reach `exec` as arguments are copied into strings the
garbage collector owns; the buffers this code holds are zeroed, the copies
cannot be. The password and IKM still appear on the child process command
line, as before, because the Monad tools take them as flags. `SECURITY.md`
has to be rewritten for this implementation before any release; it still
describes the shell mechanisms.

## Tests

`go test ./...` runs 104 test functions and needs no root, network or
systemd. The flow tests in `internal/promote` drive the real migration
against the shell suite's mock Monad binaries (`tests/mocks` on PATH) and
one local HTTP server standing in for ifconfig.me, the Foundation bucket
and the uptime API.

The four properties the review asked to see hold in Go, each with a test:

- A key or config changed after the operator confirmed it stops the
  cutover before any service is touched:
  `TestStagedKeyChangedAfterConfirmationIsRefused`,
  `TestStagedConfigChangedAfterSigningIsRefused`,
  `TestLiveFileChangedAfterSwapStopsServicesComingUp`.
- An interrupted cutover resumes safely, without repeating the swap:
  `TestPartialCutoverIsResumable`,
  `TestCrashAfterSwapBeforeStartResumesWithoutRepeatingSwap`,
  `TestFreshRunRefusedWhileCutoverInterrupted`.
- A service or API failure never becomes a false success:
  `TestServiceCrashingAfterStartIsCaughtThenResumeCompletes`,
  `TestNodeNotYetInSyncReportsPendingNotSuccess`,
  `TestUptimeAPIFailureNeverBlocks`, `TestUptimeNullLastRoundDoesNotInterruptCompletion`,
  `TestFailedBackupExportKeepsStateAndResumeRetriesOnlyExport`.
- Units the operator masked beforehand are respected:
  `TestAllPremaskedUnitsNeverCauseFalseCompletion`,
  `TestOperatorMaskedRPCIsLeftAlone`,
  `TestInterruptedMaskIsNotMistakenForTheOperators`.

The remaining flow tests cover the scenarios of `tests/promote.bats`:
abort at `STOPPED`, signer drift in both directions, differing signer
ports, every Foundation guard, the sequence floor, beneficiary handling,
node name and sequence validation, IP detection failure and the override,
manual IKM entry, the run lock, leftover state, tampered and legacy state,
the dry run, and a state file left by the shell release at step 3.

Unit tests cover the state store and its validation, file placement with a
simulated rename failure, the TOML editor against the fixture, the signer
parser against the captured v0.16.1 output, the snapshot reader against a
document shaped like the live bucket, the uptime parser, `.env` parsing,
IKM extraction, the socket-table reader and the prompts.

Not tested here: the reboot behaviour of real systemd across a
half-finished swap, the real signer and keystore binaries, and a real
migration. Those are the operator's validation runs and are not recorded
for this implementation yet.

## Not done

- `SECURITY.md` and the README for the Go build.
- A release: reproducible build recipe, checksums for the binary, signing,
  and the install instructions that go with them. The CI job builds and
  tests; it publishes nothing.
- Pointing `tests/promote.bats` at the binary. Its network scenarios rely on
  a mock `curl`, which the binary does not call; the equivalent coverage is
  in the Go flow tests.
- A test hook for the reboot procedure in `docs/reboot-test.md` (the shell
  version is edited by hand for that test).
- Version 2.0.0 itself. `--version` prints `2.0.0-dev` until a release is cut.
