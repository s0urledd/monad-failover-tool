# Validation record

## 2.0 (Go implementation)

No live-network migration is recorded for the Go implementation yet. The
records below are for the shell release. 2.0 reproduces its procedure, prompts
and safeguards; its test suite (`go test ./...`, 104 functions) drives the full
migration in-process against mock Monad binaries, including interrupted
cutovers, changed files, service and API failures and pre-masked units. That
is not a substitute for a real run. The first testnet and mainnet migrations
with 2.0 will be added here when they happen, and the VM reboot test in
`docs/reboot-test.md` has to be repeated with a `-tags reboottest` build.

The walkthrough images in the README were captured with the 1.9.4 shell
release. Apart from the version in the banner, the terminal output of 2.0 is
the same.

## Mainnet migration, September 11, 2026

The operator migrated the Huginn validator to a synced full node on Monad
mainnet using **monad-failover v1.9.4** with **Monad v0.16.2**. The operator
confirmed the Monad version separately; it is not shown in the supplied transcript.

The supplied run transcript records:

- Mainnet detection and an initial `in-sync` status with block difference 0.
- Validator backups selected from a separate directory and public keys confirmed.
- Existing beneficiary explicitly confirmed and retained.
- Foundation snapshot sequence 7, suggestion 8 accepted, and sequence 8 signed.
- Old validator confirmed stopped or offline by the operator.
- Services masked and stopped; both keys and config placed and integrity checked.
- All target services active, node in sync, and fresh key backups exported.
- `VALIDATOR PROMOTION COMPLETE`.

The operator reported no missed blocks during the transition. The transcript
does not independently measure consensus downtime or per-block participation.
Its monval result of 99.89% uptime, 927 finalized and 1 timeout covers a 24-hour
window; it does not locate that timeout within the migration or prove which
server participated. No exact downtime is claimed.

## VM reboot tests

On September 10, 2026, a disposable Ubuntu 24.04.4 VPS was used to test commit
`c7db3ea561c04cbd894d939c27b585801c8a4018` (the v1.9.2 script).

The test used real systemd units running dummy processes, synthetic key material,
and mocked Monad commands and network responses. A test copy paused after SECP
placement, before BLS and config placement. The release script itself was not
changed.

Two separate runs exercised this half-swap point:

1. `systemctl reboot`.
2. Provider-panel turn-off followed by power-on. The previous boot journal
   recorded a graceful shutdown and filesystem synchronization.

In both runs, all three units remained persistently masked and inactive after
boot, with no process start recorded for the new boot. `--resume` completed the
swap, all three live file hashes matched the saved manifest, and the dummy units
started successfully. The dummy units were stopped and disabled afterward.

These results demonstrate recovery at the tested interruption point across real
VM boots. They do not establish abrupt power-loss durability, every possible
crash window, or real Monad consensus behavior. The panel action did **not**
exercise forced power-off. See the [reboot procedure](reboot-test.md).

The tested commit predates the current release, so the two were compared function
by function. `mask_monad_services`, `unmask_monad_services`, `startable_services`,
`place_verified`, `check_live_file` and `verify_live_identity` are byte-identical
in v1.9.4. The functions that did change are `check_rpc`, `set_toml_value`,
`mode_dry_run` and `promote`, and each of those changes runs before cutover.

That keeps the mechanism these runs exercised unchanged. It is not a claim that
v1.9.4 was itself reboot-tested, and identical functions on their own do not
prove the surrounding call path is identical.

The v1.9.5 changes are covered by mocked regression tests, not another VM
reboot or mainnet migration. They change service selection/verification for
pre-existing masks and handling of optional uptime data. The v1.9.4 function
comparison above applies to that version only.

## Walkthrough images

The README still images are adapted from the operator's original terminal
screenshots, with matching frames and part of the IP covered. The first image
removes the old preflight RPC warning; the last shows the updated closing
reminder. The key-confirmation view shows the complete public keys supplied by
the operator, matching the updated tool. Added text uses glyphs taken from the terminal captures. These are
an updated walkthrough, not an exact transcript of the historical run.
The GIF presents those excerpts as a continuous scrolling walkthrough, revealing
the recorded answers at each prompt. Terminal text is copied from the images;
cursor animation and playback pauses are added to make the interaction clear.
Its timing does not measure elapsed migration time. The cutover excerpt retains
the wording from the original run, before the later wording simplification.

## Signer compatibility

A separate operator probe using a throwaway key captured output from an installed
Monad v0.16.1 signer. Its `self_address` contains the IP alone, with separate
`self_tcp_port`, `self_udp_port` and `self_auth_port` fields. The tool passes
the network's documented ports to the signer (TCP 8000, UDP 8000, authenticated
UDP 8001), reads them back from its output to confirm they round-trip, and
writes the joined address into config `self_address`. The sequence and the
signature come from the signer.

The captured format is stored in [the regression fixture](../tests/fixtures/signer-v0.16.1.out).
Fixture tests validate parsing and config handling; they do not rerun the real
binary or cryptographically validate a signature.

Monad v0.16.2 was also used in the successful mainnet migration recorded above,
including name-record signing and cutover. This is separate from the v0.16.1
throwaway-key probe. Versions beyond these two require their own compatibility
check; no separate end-to-end testnet migration is recorded here.
