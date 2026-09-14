# Verifying reboot safety on a disposable VM

The test suite proves the masking logic with mocks: it checks that the units are
masked before the swap, that a mask which does not take effect stops the run, and
that units masked beforehand stay masked. Mocks cannot prove what a real kernel
and a real systemd do across a reboot. This is the procedure for that, and it
needs a throwaway VM. Do not run it on a machine holding real validator keys.

This mechanism was exercised on a disposable Ubuntu VPS on September 10, 2026,
using real systemd services and mock Monad commands. Both an OS reboot and a
graceful panel shutdown/power-on passed. See [recorded scope and results](validation.md#vm-reboot-tests).

## Setup

1. Bring up a disposable Ubuntu VM and install a Monad full node on it, following
   the official full node installation guide. Let it sync, or stop after the
   services exist if you only want to test the swap mechanics.
2. Generate a throwaway keypair to use as the "validator" being migrated. Never
   use a real validator key for this.
3. Build a test binary with the reboot-test hook and install it in place of the
   release binary for this test only:

   ```bash
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -tags reboottest -o monad-failover ./cmd/monad-failover
   install -m 755 monad-failover /usr/local/bin/monad-failover
   ```

   The `reboottest` build tag adds a 30 second pause between the first and
   second file placement. A release build contains no such code path.

## The test

1. Start a live run and answer the prompts until you reach the `STOPPED` gate.
2. Before typing `STOPPED`, open a second shell and confirm the units are enabled:

   ```bash
   systemctl is-enabled monad-bft monad-execution monad-rpc
   ```

3. Type `STOPPED` and confirm the cutover. The test build prints
   `reboottest build: pausing 30s after the SECP placement`; kill the run
   during that pause, so the node is left with the new SECP key and the old
   BLS key and config.
4. With the run killed mid-swap, confirm the units are masked:

   ```bash
   systemctl is-enabled monad-bft monad-execution monad-rpc   # expect: masked
   ```

5. Reboot the VM.
6. After it comes back, confirm the services did **not** start:

   ```bash
   systemctl is-active monad-bft monad-execution monad-rpc    # expect: inactive
   journalctl -u monad-bft -b | tail
   ```

   This is the property being tested. Without the mask the units would have
   started here with one new key and one old one.

7. Finish the migration:

   ```bash
   monad-failover --resume
   ```

8. Confirm the units are unmasked and active, and that the identity is
   consistent (both keys and `node.toml` are the new ones). Then reinstall the
   release binary.

## What a pass looks like

- Units report `masked` while the swap is unfinished.
- Nothing starts after the reboot.
- `--resume` completes the swap, unmasks, starts the services, and the node comes
  up with a single consistent identity.

Record the systemd output from steps 4 and 6; that is the evidence. A green test
suite is not a substitute for it.
